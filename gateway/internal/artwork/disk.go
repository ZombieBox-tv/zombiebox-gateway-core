package artwork

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Cache stores only processed images. An unreadable/disabled cache is a miss.
type Cache interface {
	Get([32]byte) ([]byte, time.Time, bool)
	Put([32]byte, []byte, time.Time)
}

// DiskCache is owned by one gateway process. Files carry an absolute expiry;
// access timestamps (mtime) drive eviction without extending freshness.
type DiskCache struct {
	directory string
	maxBytes  int64
	mu        sync.Mutex
}

func NewDiskCache(directory string, maxBytes int64) (*DiskCache, error) {
	if maxBytes < 256<<10 || maxBytes > 512<<20 {
		return nil, errors.New("artwork disk budget must be between 256 KiB and 512 MiB")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0700); err != nil {
		return nil, err
	}
	c := &DiskCache{directory: directory, maxBytes: maxBytes}
	c.prune(time.Now())
	return c, nil
}

func (c *DiskCache) path(key [32]byte) string {
	return filepath.Join(c.directory, hex.EncodeToString(key[:])+".cache")
}

func (c *DiskCache) Get(key [32]byte) ([]byte, time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	path := c.path(key)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 8 || info.Size() > (256<<10)+8 {
		return nil, time.Time{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (256<<10)+9))
	if err != nil || len(data) <= 8 || len(data) > (256<<10)+8 {
		return nil, time.Time{}, false
	}
	expires := time.Unix(int64(binary.BigEndian.Uint64(data[:8])), 0)
	now := time.Now()
	if !now.Before(expires) || len(data) < 10 || data[8] != 0xff || data[9] != 0xd8 {
		_ = os.Remove(path)
		return nil, time.Time{}, false
	}
	_ = os.Chtimes(path, now, now)
	return data[8:], expires, true
}

func (c *DiskCache) Put(key [32]byte, data []byte, expires time.Time) {
	if len(data) == 0 || len(data) > 256<<10 || !time.Now().Before(expires) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	file, err := os.CreateTemp(c.directory, ".artwork-*")
	if err != nil {
		return // Disk pressure must not break an otherwise usable image response.
	}
	defer os.Remove(file.Name())
	header := make([]byte, 8)
	binary.BigEndian.PutUint64(header, uint64(expires.Unix()))
	_, err = file.Write(append(header, data...))
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return
	}
	if os.Rename(file.Name(), c.path(key)) == nil {
		c.prune(time.Now())
	}
}

func (c *DiskCache) prune(now time.Time) {
	entries, err := os.ReadDir(c.directory)
	if err != nil {
		return
	}
	var files []os.FileInfo
	var total int64
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".artwork-") {
			_ = os.Remove(filepath.Join(c.directory, name))
			continue
		}
		if len(name) != 70 || !strings.HasSuffix(name, ".cache") {
			continue
		}
		if _, err := hex.DecodeString(name[:64]); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		path := filepath.Join(c.directory, name)
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		var header [8]byte
		_, err = io.ReadFull(file, header[:])
		_ = file.Close()
		expires := time.Unix(int64(binary.BigEndian.Uint64(header[:])), 0)
		if err != nil || !now.Before(expires) || info.Size() <= 8 || info.Size() > (256<<10)+8 {
			_ = os.Remove(path)
			continue
		}
		files = append(files, info)
		total += info.Size()
	}
	sort.Slice(files, func(a, b int) bool { return files[a].ModTime().Before(files[b].ModTime()) })
	for index, file := range files {
		if total <= c.maxBytes && len(files)-index <= 1024 {
			break
		}
		if os.Remove(filepath.Join(c.directory, file.Name())) == nil {
			total -= file.Size()
		}
	}
}
