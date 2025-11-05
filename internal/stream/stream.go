package stream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
    	"path/filepath"
    	"strings"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/buffer"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/rclone/rclone/lib/mmap"
	"go4.org/readerutil"
)

type FileStream struct {
	Ctx context.Context
	model.Obj
	io.Reader
	Mimetype          string
	WebPutAsTask      bool
	ForceStreamUpload bool
	Exist             model.Obj //the file existed in the destination, we can reuse some info since we wil overwrite it
	utils.Closers

	tmpFile   model.File //if present, tmpFile has full content, it will be deleted at last
	peekBuff  *buffer.Reader
	size      int64
	oriReader io.Reader // the original reader, used for caching
	link *model.Link
}

func (f *FileStream) GetSize() int64 {
	if f.size > 0 {
		return f.size
	}
	if file, ok := f.tmpFile.(*os.File); ok {
		info, err := file.Stat()
		if err == nil {
			return info.Size()
		}
	}
	return f.Obj.GetSize()
}

func (f *FileStream) GetMimetype() string {
	return f.Mimetype
}

func (f *FileStream) NeedStore() bool {
	return f.WebPutAsTask
}

func (f *FileStream) IsForceStreamUpload() bool {
	return f.ForceStreamUpload
}

func (f *FileStream) Close() error {
	if f.peekBuff != nil {
		f.peekBuff.Reset()
		f.peekBuff = nil
	}

	var err1, err2 error
	err1 = f.Closers.Close()
	if errors.Is(err1, os.ErrClosed) {
		err1 = nil
	}
	if file, ok := f.tmpFile.(*os.File); ok {
		err2 = os.RemoveAll(file.Name())
		if err2 != nil {
			err2 = errs.NewErr(err2, "failed to remove tmpFile [%s]", file.Name())
		} else {
			f.tmpFile = nil
		}
	}

	return errors.Join(err1, err2)
}

func (f *FileStream) GetExist() model.Obj {
	return f.Exist
}
func (f *FileStream) SetExist(obj model.Obj) {
	f.Exist = obj
}

// CacheFullAndWriter save all data into tmpFile or memory.
// It's not thread-safe!
func (f *FileStream) CacheFullAndWriter(up *model.UpdateProgress, writer io.Writer) (model.File, error) {
	if f.needIsoPartialCache() {
        	return f.cacheIsoMetadata(up, writer)
    	}
	if cache := f.GetFile(); cache != nil {
		if writer == nil {
			return cache, nil
		}
		_, err := cache.Seek(0, io.SeekStart)
		if err == nil {
			reader := f.Reader
			if up != nil {
				cacheProgress := model.UpdateProgressWithRange(*up, 0, 50)
				*up = model.UpdateProgressWithRange(*up, 50, 100)
				reader = &ReaderUpdatingProgress{
					Reader: &SimpleReaderWithSize{
						Reader: reader,
						Size:   f.GetSize(),
					},
					UpdateProgress: cacheProgress,
				}
			}
			_, err = utils.CopyWithBuffer(writer, reader)
			if err == nil {
				_, err = cache.Seek(0, io.SeekStart)
			}
		}
		if err != nil {
			return nil, err
		}
		return cache, nil
	}

	reader := f.Reader
	if up != nil {
		cacheProgress := model.UpdateProgressWithRange(*up, 0, 50)
		*up = model.UpdateProgressWithRange(*up, 50, 100)
		reader = &ReaderUpdatingProgress{
			Reader: &SimpleReaderWithSize{
				Reader: reader,
				Size:   f.GetSize(),
			},
			UpdateProgress: cacheProgress,
		}
	}
	if writer != nil {
		reader = io.TeeReader(reader, writer)
	}

	if f.GetSize() < 0 {
		if f.peekBuff == nil {
			f.peekBuff = &buffer.Reader{}
		}
		// 检查是否有数据
		buf := []byte{0}
		n, err := io.ReadFull(reader, buf)
		if n > 0 {
			f.peekBuff.Append(buf[:n])
		}
		if err == io.ErrUnexpectedEOF {
			f.size = f.peekBuff.Size()
			f.Reader = f.peekBuff
			return f.peekBuff, nil
		} else if err != nil {
			return nil, err
		}
		if conf.MaxBufferLimit-n > conf.MmapThreshold && conf.MmapThreshold > 0 {
			m, err := mmap.Alloc(conf.MaxBufferLimit - n)
			if err == nil {
				f.Add(utils.CloseFunc(func() error {
					return mmap.Free(m)
				}))
				n, err = io.ReadFull(reader, m)
				if n > 0 {
					f.peekBuff.Append(m[:n])
				}
				if err == io.ErrUnexpectedEOF {
					f.size = f.peekBuff.Size()
					f.Reader = f.peekBuff
					return f.peekBuff, nil
				} else if err != nil {
					return nil, err
				}
			}
		}

		tmpF, err := utils.CreateTempFile(reader, 0)
		if err != nil {
			return nil, err
		}
		f.Add(utils.CloseFunc(func() error {
			return errors.Join(tmpF.Close(), os.RemoveAll(tmpF.Name()))
		}))
		peekF, err := buffer.NewPeekFile(f.peekBuff, tmpF)
		if err != nil {
			return nil, err
		}
		f.size = peekF.Size()
		f.Reader = peekF
		return peekF, nil
	}

	f.Reader = reader
	return f.cache(f.GetSize())
}

func (f *FileStream) GetFile() model.File {
	if f.tmpFile != nil {
		return f.tmpFile
	}
	if file, ok := f.Reader.(model.File); ok {
		return file
	}
	return nil
}

// RangeRead have to cache all data first since only Reader is provided.
// It's not thread-safe!
func (f *FileStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	if httpRange.Length < 0 || httpRange.Start+httpRange.Length > f.GetSize() {
		httpRange.Length = f.GetSize() - httpRange.Start
	}
	if f.GetFile() != nil {
		return io.NewSectionReader(f.GetFile(), httpRange.Start, httpRange.Length), nil
	}

	size := httpRange.Start + httpRange.Length
	if f.peekBuff != nil && size <= int64(f.peekBuff.Size()) {
		return io.NewSectionReader(f.peekBuff, httpRange.Start, httpRange.Length), nil
	}

	cache, err := f.cache(size)
	if err != nil {
		return nil, err
	}

	return io.NewSectionReader(cache, httpRange.Start, httpRange.Length), nil
}

// *旧笔记
// 使用bytes.Buffer作为io.CopyBuffer的写入对象，CopyBuffer会调用Buffer.ReadFrom
// 即使被写入的数据量与Buffer.Cap一致，Buffer也会扩大

func (f *FileStream) cache(maxCacheSize int64) (model.File, error) {
	if maxCacheSize > int64(conf.MaxBufferLimit) {
		tmpF, err := utils.CreateTempFile(f.Reader, f.GetSize())
		if err != nil {
			return nil, err
		}
		f.Add(tmpF)
		f.tmpFile = tmpF
		f.Reader = tmpF
		return tmpF, nil
	}

	if f.peekBuff == nil {
		f.peekBuff = &buffer.Reader{}
		f.oriReader = f.Reader
	}
	bufSize := maxCacheSize - int64(f.peekBuff.Size())
	var buf []byte
	if conf.MmapThreshold > 0 && bufSize >= int64(conf.MmapThreshold) {
		m, err := mmap.Alloc(int(bufSize))
		if err == nil {
			f.Add(utils.CloseFunc(func() error {
				return mmap.Free(m)
			}))
			buf = m
		}
	}
	if buf == nil {
		buf = make([]byte, bufSize)
	}
	n, err := io.ReadFull(f.oriReader, buf)
	if bufSize != int64(n) {
		return nil, fmt.Errorf("failed to read all data: (expect =%d, actual =%d) %w", bufSize, n, err)
	}
	f.peekBuff.Append(buf)
	if int64(f.peekBuff.Size()) >= f.GetSize() {
		f.Reader = f.peekBuff
		f.oriReader = nil
	} else {
		f.Reader = io.MultiReader(f.peekBuff, f.oriReader)
	}
	return f.peekBuff, nil
}

func (f *FileStream) SetTmpFile(file model.File) {
	f.AddIfCloser(file)
	f.tmpFile = file
	f.Reader = file
}

var _ model.FileStreamer = (*SeekableStream)(nil)
var _ model.FileStreamer = (*FileStream)(nil)

//var _ seekableStream = (*FileStream)(nil)

// for most internal stream, which is either RangeReadCloser or MFile
// Any functionality implemented based on SeekableStream should implement a Close method,
// whose only purpose is to close the SeekableStream object. If such functionality has
// additional resources that need to be closed, they should be added to the Closer property of
// the SeekableStream object and be closed together when the SeekableStream object is closed.
type SeekableStream struct {
	*FileStream
	// should have one of belows to support rangeRead
	rangeReadCloser model.RangeReadCloserIF
}

func NewSeekableStream(fs *FileStream, link *model.Link) (*SeekableStream, error) {
	if len(fs.Mimetype) == 0 {
		fs.Mimetype = utils.GetMimeType(fs.Obj.GetName())
	}

	if fs.Reader != nil {
		fs.Add(link)
		return &SeekableStream{FileStream: fs}, nil
	}

	if link != nil {
		size := link.ContentLength
		if size <= 0 {
			size = fs.GetSize()
		}
		rr, err := GetRangeReaderFromLink(size, link)
		if err != nil {
			return nil, err
		}
		rrc := &model.RangeReadCloser{
			RangeReader: rr,
		}
		if _, ok := rr.(*model.FileRangeReader); ok {
			fs.Reader, err = rrc.RangeRead(fs.Ctx, http_range.Range{Length: -1})
			if err != nil {
				return nil, err
			}
		}
		fs.size = size
		fs.Add(link)
		fs.Add(rrc)
		return &SeekableStream{FileStream: fs, rangeReadCloser: rrc}, nil
	}
	return nil, fmt.Errorf("illegal seekableStream")
}

// RangeRead is not thread-safe, pls use it in single thread only.
func (ss *SeekableStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	if ss.GetFile() == nil && ss.rangeReadCloser != nil {
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, httpRange)
		if err != nil {
			return nil, err
		}
		return rc, nil
	}
	return ss.FileStream.RangeRead(httpRange)
}

// only provide Reader as full stream when it's demanded. in rapid-upload, we can skip this to save memory
func (ss *SeekableStream) Read(p []byte) (n int, err error) {
	if err := ss.generateReader(); err != nil {
		return 0, err
	}
	return ss.FileStream.Read(p)
}

func (ss *SeekableStream) generateReader() error {
	if ss.Reader == nil {
		if ss.rangeReadCloser == nil {
			return fmt.Errorf("illegal seekableStream")
		}
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, http_range.Range{Length: -1})
		if err != nil {
			return err
		}
		ss.Reader = rc
	}
	return nil
}

func (ss *SeekableStream) CacheFullAndWriter(up *model.UpdateProgress, writer io.Writer) (model.File, error) {
	if err := ss.generateReader(); err != nil {
		return nil, err
	}
	return ss.FileStream.CacheFullAndWriter(up, writer)
}

type ReaderWithSize interface {
	io.Reader
	GetSize() int64
}

type SimpleReaderWithSize struct {
	io.Reader
	Size int64
}

func (r *SimpleReaderWithSize) GetSize() int64 {
	return r.Size
}

func (r *SimpleReaderWithSize) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type ReaderUpdatingProgress struct {
	Reader ReaderWithSize
	model.UpdateProgress
	offset int
}

func (r *ReaderUpdatingProgress) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.offset += n
	r.UpdateProgress(math.Min(100.0, float64(r.offset)/float64(r.Reader.GetSize())*100.0))
	return n, err
}

func (r *ReaderUpdatingProgress) Close() error {
	if c, ok := r.Reader.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

type RangeReadReadAtSeeker struct {
	ss        *SeekableStream
	masterOff int64
	readerMap sync.Map
	headCache *headCache
}

type headCache struct {
	reader io.Reader
	bufs   [][]byte
}

func (c *headCache) head(p []byte) (int, error) {
	n := 0
	for _, buf := range c.bufs {
		n += copy(p[n:], buf)
		if n == len(p) {
			return n, nil
		}
	}
	nn, err := io.ReadFull(c.reader, p[n:])
	if nn > 0 {
		buf := make([]byte, nn)
		copy(buf, p[n:])
		c.bufs = append(c.bufs, buf)
		n += nn
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
	}
	return n, err
}

func (r *headCache) Close() error {
	clear(r.bufs)
	r.bufs = nil
	return nil
}

func (r *RangeReadReadAtSeeker) InitHeadCache() {
	if r.ss.GetFile() == nil && r.masterOff == 0 {
		value, _ := r.readerMap.LoadAndDelete(int64(0))
		r.headCache = &headCache{reader: value.(io.Reader)}
		r.ss.Closers.Add(r.headCache)
	}
}

func NewReadAtSeeker(ss *SeekableStream, offset int64, forceRange ...bool) (model.File, error) {
	if ss.GetFile() != nil {
		_, err := ss.GetFile().Seek(offset, io.SeekStart)
		if err != nil {
			return nil, err
		}
		return ss.GetFile(), nil
	}
	r := &RangeReadReadAtSeeker{
		ss:        ss,
		masterOff: offset,
	}
	if offset != 0 || utils.IsBool(forceRange...) {
		if offset < 0 || offset > ss.GetSize() {
			return nil, errors.New("offset out of range")
		}
		_, err := r.getReaderAtOffset(offset)
		if err != nil {
			return nil, err
		}
	} else {
		r.readerMap.Store(int64(offset), ss)
	}
	return r, nil
}

func NewMultiReaderAt(ss []*SeekableStream) (readerutil.SizeReaderAt, error) {
	readers := make([]readerutil.SizeReaderAt, 0, len(ss))
	for _, s := range ss {
		ra, err := NewReadAtSeeker(s, 0)
		if err != nil {
			return nil, err
		}
		readers = append(readers, io.NewSectionReader(ra, 0, s.GetSize()))
	}
	return readerutil.NewMultiReaderAt(readers...), nil
}

func (r *RangeReadReadAtSeeker) getReaderAtOffset(off int64) (io.Reader, error) {
	var rr io.Reader
	var cur int64 = -1
	r.readerMap.Range(func(key, value any) bool {
		k := key.(int64)
		if off == k {
			cur = k
			rr = value.(io.Reader)
			return false
		}
		if off > k && off-k <= 4*utils.MB && (rr == nil || k < cur) {
			rr = value.(io.Reader)
			cur = k
		}
		return true
	})
	if cur >= 0 {
		r.readerMap.Delete(int64(cur))
	}
	if off == int64(cur) {
		// logrus.Debugf("getReaderAtOffset match_%d", off)
		return rr, nil
	}

	if rr != nil {
		n, _ := utils.CopyWithBufferN(io.Discard, rr, off-cur)
		cur += n
		if cur == off {
			// logrus.Debugf("getReaderAtOffset old_%d", off)
			return rr, nil
		}
	}
	// logrus.Debugf("getReaderAtOffset new_%d", off)

	reader, err := r.ss.RangeRead(http_range.Range{Start: off, Length: -1})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *RangeReadReadAtSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= r.ss.GetSize() {
		return 0, io.EOF
	}
	if off == 0 && r.headCache != nil {
		return r.headCache.head(p)
	}
	var rr io.Reader
	rr, err = r.getReaderAtOffset(off)
	if err != nil {
		return 0, err
	}
	n, err = io.ReadFull(rr, p)
	if n > 0 {
		off += int64(n)
		switch err {
		case nil:
			r.readerMap.Store(int64(off), rr)
		case io.ErrUnexpectedEOF:
			err = io.EOF
		}
	}
	return n, err
}

func (r *RangeReadReadAtSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.masterOff
	case io.SeekEnd:
		offset += r.ss.GetSize()
	default:
		return 0, errors.New("Seek: invalid whence")
	}
	if offset < 0 || offset > r.ss.GetSize() {
		return 0, errors.New("Seek: invalid offset")
	}
	r.masterOff = offset
	return offset, nil
}

func (r *RangeReadReadAtSeeker) Read(p []byte) (n int, err error) {
	n, err = r.ReadAt(p, r.masterOff)
	if n > 0 {
		r.masterOff += int64(n)
	}
	return n, err
}

// 判断是否需要ISO元数据部分缓存
func (f *FileStream) needIsoPartialCache() bool {
    if !strings.HasSuffix(strings.ToLower(f.GetName()), ".iso") {
        return false
    }

    // 检查是否支持Range请求（通过Link判断）
    if f.link != nil && (f.link.RangeReader != nil || f.link.Concurrency > 0) {
        return true
    }

    // 检查是否已存在完整缓存
    if f.GetFile() != nil {
        return false
    }

    return true
}

// 缓存ISO前16M元数据并持久化
func (f *FileStream) cacheIsoMetadata(up *model.UpdateProgress, writer io.Writer) (model.File, error) {
    const isoMetaSize = 16 * 1024 * 1024 // 16M
    var cacheFile model.File
    var err error

    // 尝试加载已持久化的元数据缓存
    cacheFile, err = f.loadPersistedIsoCache()
    if err == nil && cacheFile != nil {
        return cacheFile, nil
    }

    // 创建临时文件存储元数据
    tmpF, err := os.CreateTemp(conf.Conf.TempDir, "iso-meta-*")
    if err != nil {
        return nil, err
    }
    defer func() {
        if cacheFile == nil {
            _ = tmpF.Close()
            _ = os.Remove(tmpF.Name())
        }
    }()

    // 限制只读取前16M
    limitedReader := io.LimitReader(f.Reader, isoMetaSize)
    if up != nil {
        limitedReader = &ReaderUpdatingProgress{
	    Reader: &SimpleReaderWithSize{
		Reader: limitedReader,
		Size:   isoMetaSize,
	    },
	    UpdateProgress: *up,
        }
    }

    // 同时写入缓存文件和目标writer
    tee := io.TeeReader(limitedReader, tmpF)
    if writer != nil {
        _, err = io.Copy(writer, tee)
    } else {
        _, err = io.Copy(io.Discard, tee)
    }
    if err != nil && !errors.Is(err, io.EOF) {
        return nil, err
    }

    // 持久化缓存文件（移动到专用目录）
    metaCacheDir := filepath.Join(conf.Conf.TempDir, "iso-meta")
    if err := os.MkdirAll(metaCacheDir, 0755); err != nil {
        return nil, err
    }

    // 使用文件哈希作为缓存文件名
    hash := f.calculateCacheKey()
    destPath := filepath.Join(metaCacheDir, hash)
    if err := os.Rename(tmpF.Name(), destPath); err != nil {
        return nil, err
    }

    // 重新打开持久化的缓存文件
    cachedFile, err := os.Open(destPath)
    if err != nil {
        return nil, err
    }

    // 记录缓存文件信息以便后续清理
    f.SetTmpFile(cachedFile)
    f.Add(utils.CloseFunc(func() error {
        return cachedFile.Close()
    }))

    return cachedFile, nil
}

// 计算ISO缓存的唯一标识
func (f *FileStream) calculateCacheKey() string {
    // 使用文件哈希和大小作为缓存键
    hash := ""
    for _, v := range f.GetHash().Export() {
        if v > hash {
            hash = v
        }
    }
    if hash == "" {
        hash = fmt.Sprintf("%x-%x", f.ModTime().Unix(), f.GetSize())
    }
    return hash
}

// 加载已持久化的ISO元数据缓存
func (f *FileStream) loadPersistedIsoCache() (model.File, error) {
    hash := f.calculateCacheKey()
    metaCacheDir := filepath.Join(conf.Conf.TempDir, "iso-meta")
    cachePath := filepath.Join(metaCacheDir, hash)

    // 检查缓存文件是否存在且大小正确
    info, err := os.Stat(cachePath)
    if err != nil {
        return nil, err
    }

    if info.Size() != 16*1024*1024 {
        _ = os.Remove(cachePath)
        return nil, errors.New("invalid iso meta cache size")
    }

    return os.Open(cachePath)
}
