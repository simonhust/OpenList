package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"go4.org/readerutil"
)

// Check if the file is UDF related (ISO, Blu-ray images, etc.)
func IsUDFType(filename string) bool {
	ext := strings.ToLower(utils.GetFileExt(filename))
	return ext == ".iso" || ext == ".img" || strings.Contains(strings.ToLower(filename), "bluray")
}

type FileStream struct {
	Ctx context.Context
	model.Obj
	io.Reader
	Mimetype          string
	WebPutAsTask      bool
	ForceStreamUpload bool
	Exist             model.Obj // Existing file in target that can reuse some information
	utils.Closers
	tmpFile    *os.File // If exists, contains complete content and will be deleted eventually
	peekBuff   *bytes.Reader
	rangeCache sync.Map // Caches small range read results, key: "start-end", value: cachedItem
}

type cachedItem struct {
	reader    io.Reader
	isUDFMeta bool // Whether this is UDF metadata
}

func (f *FileStream) GetSize() int64 {
	if f.tmpFile != nil {
		info, err := f.tmpFile.Stat()
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
	var err1, err2 error

	// Clean up memory cache (critical modification: release all caches when closing)
	f.peekBuff = nil
	f.rangeCache = sync.Map{} // Clear range cache

	// Original closing logic
	err1 = f.Closers.Close()
	if errors.Is(err1, os.ErrClosed) {
		err1 = nil
	}
	if f.tmpFile != nil {
		err2 = os.RemoveAll(f.tmpFile.Name())
		if err2 != nil {
			err2 = errs.NewErr(err2, "failed to remove tmpFile [%s]", f.tmpFile.Name())
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

// CacheFullInTempFile saves all data to a temporary file. Not recommended as it wears out the disk,
// and upload can only start after file writing is complete. Not thread-safe!
func (f *FileStream) CacheFullInTempFile() (model.File, error) {
	// Disable automatic full caching for UDF files (unless explicitly required)
	if IsUDFType(f.Obj.GetName()) && !f.ForceStreamUpload {
		return nil, fmt.Errorf("skip full cache for UDF file")
	}

	if file := f.GetFile(); file != nil {
		return file, nil
	}
	tmpF, err := utils.CreateTempFile(f.Reader, f.GetSize())
	if err != nil {
		return nil, err
	}
	f.Add(tmpF)
	f.tmpFile = tmpF
	f.Reader = tmpF
	return tmpF, nil
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

// RangeRead must first cache all data since only a Reader is provided.
// Also supports peeking RangeRead at the start position, but in-memory buffered data will not exceed conf.MaxBufferLimit
func (f *FileStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	// 1. Identify UDF related files
	isUDF := IsUDFType(f.Obj.GetName())

	// 2. Handle priority caching for UDF file metadata regions
	if isUDF {
		// Adjust cache threshold: relax to 150MB for UDF files
		adjustedLimit := max(int64(conf.MaxBufferLimit), 150*utils.MB)
		
		// Calculate actual required range
		if httpRange.Length < 0 || httpRange.Start+httpRange.Length > f.GetSize() {
			httpRange.Length = f.GetSize() - httpRange.Start
		}
		end := httpRange.Start + httpRange.Length

		// Force in-memory caching if request is within UDF metadata region (first 150MB)
		if httpRange.Start < 150*utils.MB {
			// Ensure cache covers required range
			if f.peekBuff == nil || int64(f.peekBuff.Len()) < end {
				neededSize := end
				currentSize := int64(0)
				var existingBuf []byte

				// Read existing cache
				if f.peekBuff != nil {
					currentSize = int64(f.peekBuff.Len())
					existingBuf = make([]byte, currentSize)
					if _, err := f.peekBuff.Read(existingBuf); err != nil && !errors.Is(err, io.EOF) {
						return nil, fmt.Errorf("read existing buffer failed: %w", err)
					}
					// Reset peekBuff's read position
					f.peekBuff.Seek(0, io.SeekStart)
				}

				// Read missing portion
				readSize := neededSize - currentSize
				if readSize > 0 {
					newBuf := make([]byte, readSize)
					n, err := io.ReadFull(f.Reader, newBuf)
					if err != nil && !errors.Is(err, io.EOF) {
						return nil, fmt.Errorf("read UDF data failed: %w", err)
					}
					// Merge caches
					existingBuf = append(existingBuf, newBuf[:n]...)
					currentSize += int64(n)
				}

				// Update in-memory cache
				f.peekBuff = bytes.NewReader(existingBuf)
				f.Reader = io.MultiReader(f.peekBuff, f.Reader)
			}

			// Return requested range from memory cache
			return io.NewSectionReader(f.peekBuff, httpRange.Start, httpRange.Length), nil
		}
	}

	// For non-UDF files or beyond metadata region, use original logic
	if httpRange.Length < 0 || httpRange.Start+httpRange.Length > f.GetSize() {
		httpRange.Length = f.GetSize() - httpRange.Start
	}
	var cache io.ReaderAt = f.GetFile()
	if cache != nil {
		return io.NewSectionReader(cache, httpRange.Start, httpRange.Length), nil
	}

	if f.peekBuff != nil && httpRange.Start+httpRange.Length <= int64(f.peekBuff.Len()) {
		return io.NewSectionReader(f.peekBuff, httpRange.Start, httpRange.Length), nil
	}
	if httpRange.Start+httpRange.Length <= int64(conf.MaxBufferLimit) {
		bufSize := min(httpRange.Start+httpRange.Length, f.GetSize())
		buf := make([]byte, bufSize)
		n, err := io.ReadFull(f.Reader, buf)
		if err != nil {
			return nil, fmt.Errorf("failed to read all data: (expect =%d, actual =%d) %w", bufSize, n, err)
		}
		f.peekBuff = bytes.NewReader(buf)
		f.Reader = io.MultiReader(f.peekBuff, f.Reader)
		cache = f.peekBuff
	} else {
		var err error
		cache, err = f.CacheFullInTempFile()
		if err != nil {
			return nil, err
		}
	}
	return io.NewSectionReader(cache, httpRange.Start, httpRange.Length), nil
}

var _ model.FileStreamer = (*SeekableStream)(nil)
var _ model.FileStreamer = (*FileStream)(nil)

// For most internal streams, they are either RangeReadCloser or MFile
// Any functionality implemented based on SeekableStream should implement the Close method,
// whose sole purpose is to close the SeekableStream object. If such functionality has
// additional resources that need closing, they should be added to the Closer property of the SeekableStream object,
// and closed together when the SeekableStream object is closed.
type SeekableStream struct {
	*FileStream
	rangeReadCloser model.RangeReadCloserIF
	size            int64
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
		if _, ok := rr.(*model.FileRangeReader); ok {
			fs.Reader, err = rr.RangeRead(fs.Ctx, http_range.Range{Length: -1})
			if err != nil {
				return nil, err
			}
			fs.Add(link)
			return &SeekableStream{FileStream: fs, size: size}, nil
		}
		rrc := &model.RangeReadCloser{
			RangeReader: rr,
		}
		fs.Add(link)
		fs.Add(rrc)
		ss := &SeekableStream{FileStream: fs, rangeReadCloser: rrc, size: size}
		
		// Preload critical metadata regions for UDF files
		if IsUDFType(fs.Obj.GetName()) {
			go ss.preloadUDFMetadata()
		}
		
		return ss, nil
	}
	return nil, fmt.Errorf("illegal seekableStream")
}

// Preload critical metadata regions for UDF files
func (ss *SeekableStream) preloadUDFMetadata() {
	ctx, cancel := context.WithTimeout(ss.Ctx, 30*time.Second)
	defer cancel()

	// Phased preloading of critical regions
	ranges := []http_range.Range{
		{Start: 0, Length: 2 * utils.MB},          // Volume descriptors
		{Start: 2 * utils.MB, Length: 48 * utils.MB}, // Root directory and BDMV core
		{Start: 50 * utils.MB, Length: 100 * utils.MB}, // Extended metadata
	}

	for _, r := range ranges {
		select {
		case <-ctx.Done():
			return
		default:
			// Preload and cache
			if _, err := ss.RangeRead(r); err != nil {
				// Preloading failure doesn't affect main process, only log
				fmt.Printf("preload UDF range %d-%d failed: %v\n", r.Start, r.Start+r.Length, err)
			}
		}
	}
}

func (ss *SeekableStream) GetSize() int64 {
	if ss.size > 0 {
		return ss.size
	}
	return ss.FileStream.GetSize()
}

// RangeRead is not thread-safe, please use only in single thread.
func (ss *SeekableStream) RangeRead(httpRange http_range.Range) (io.Reader, error) {
	isUDF := IsUDFType(ss.Obj.GetName())
	
	// For small range reads (<1MB) on UDF files, cache in memory to avoid repeated requests
	if isUDF && httpRange.Length > 0 && httpRange.Length < 1*utils.MB {
		cacheKey := fmt.Sprintf("%d-%d", httpRange.Start, httpRange.Start+httpRange.Length)
		
		// Check if cache exists
		if cached, ok := ss.rangeCache.Load(cacheKey); ok {
			return cached.(cachedItem).reader, nil
		}

		// Read and cache
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, httpRange)
		if err != nil {
			return nil, err
		}
		
		// Read small range data into memory cache
		buf, err := io.ReadAll(rc)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		cachedReader := bytes.NewReader(buf)
		ss.rangeCache.Store(cacheKey, cachedItem{
			reader:    cachedReader,
			isUDFMeta: httpRange.Start < 150*utils.MB, // Mark as metadata cache
		})

		// Limit cache size, only clean non-metadata cache
		if ss.rangeCache.Len() > 200 {
			var toDelete []interface{}
			ss.rangeCache.Range(func(key, value any) bool {
				item := value.(cachedItem)
				if !item.isUDFMeta {
					toDelete = append(toDelete, key)
				}
				return true
			})
			// Delete some normal cache
			for i, key := range toDelete {
				if i >= 100 {
					break
				}
				ss.rangeCache.Delete(key)
			}
		}
		return cachedReader, nil
	}

	// Original logic
	if ss.tmpFile == nil && ss.rangeReadCloser != nil {
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, httpRange)
		if err != nil {
			return nil, err
		}
		return rc, nil
	}
	return ss.FileStream.RangeRead(httpRange)
}

func (ss *SeekableStream) Read(p []byte) (n int, err error) {
	if ss.Reader == nil {
		if ss.rangeReadCloser == nil {
			return 0, fmt.Errorf("illegal seekableStream")
		}
		rc, err := ss.rangeReadCloser.RangeRead(ss.Ctx, http_range.Range{Length: -1})
		if err != nil {
			return 0, err
		}
		ss.Reader = rc
	}
	return ss.Reader.Read(p)
}

func (ss *SeekableStream) CacheFullInTempFile() (model.File, error) {
	// Disable automatic full caching for UDF files (unless explicitly required)
	if IsUDFType(ss.Obj.GetName()) && !ss.ForceStreamUpload {
		return nil, fmt.Errorf("skip full cache for UDF file")
	}

	if file := ss.GetFile(); file != nil {
		return file, nil
	}
	tmpF, err := utils.CreateTempFile(ss, ss.GetSize())
	if err != nil {
		return nil, err
	}
	ss.Add(tmpF)
	ss.tmpFile = tmpF
	ss.Reader = tmpF
	return tmpF, nil
}

func (f *FileStream) SetTmpFile(r *os.File) {
	f.Add(r)
	f.tmpFile = r
	f.Reader = r
}

type ReaderWithSize interface {
	io.ReadCloser
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
	return r.Reader.Close()
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
		if len(buf)+n >= len(p) {
			n += copy(p[n:], buf[:len(p)-n])
			return n, nil
		} else {
			n += copy(p[n:], buf)
		}
	}
	w, err := io.ReadAtLeast(c.reader, p[n:], 1)
	if w > 0 {
		buf := make([]byte, w)
		copy(buf, p[n:n+w])
		c.bufs = append(c.bufs, buf)
		n += w
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
		return rr, nil
	}

	if rr != nil {
		n, _ := utils.CopyWithBufferN(io.Discard, rr, off-cur)
		cur += n
		if cur == off {
			return rr, nil
		}
	}

	reader, err := r.ss.RangeRead(http_range.Range{Start: off, Length: -1})
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *RangeReadReadAtSeeker) ReadAt(p []byte, off int64) (n int, err error) {
	if off == 0 && r.headCache != nil {
		return r.headCache.head(p)
	}
	var rr io.Reader
	rr, err = r.getReaderAtOffset(off)
	if err != nil {
		return 0, err
	}
	n, err = io.ReadAtLeast(rr, p, 1)
	off += int64(n)
	if err == nil {
		r.readerMap.Store(int64(off), rr)
	} else {
		rr = nil
	}
	return n, err
}

func (r *RangeReadReadAtSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		if offset == 0 {
			return r.masterOff, nil
		}
		offset += r.masterOff
	case io.SeekEnd:
		offset += r.ss.GetSize()
	default:
		return 0, errs.NotSupport
	}
	if offset < 0 {
		return r.masterOff, errors.New("invalid seek: negative position")
	}
	if offset > r.ss.GetSize() {
		offset = r.ss.GetSize()
	}
	r.masterOff = offset
	return offset, nil
}

func (r *RangeReadReadAtSeeker) Read(p []byte) (n int, err error) {
	n, err = r.ReadAt(p, r.masterOff)
	r.masterOff += int64(n)
	return n, err
}

// Helper function: get minimum of two int64 values
func min(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// Helper function: get maximum of two int64 values
func max(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
    
