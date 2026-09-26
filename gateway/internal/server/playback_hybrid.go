package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"zombiebox.local/gateway/internal/media"
)

var (
	ErrSpoolQuotaExceeded      = errors.New("spool_quota_exceeded")
	ErrSpoolLimitExceeded      = errors.New("spool_limit_exceeded")
	ErrSpoolStorageUnavailable = errors.New("spool_storage_unavailable")
)

const (
	defaultMaxHybridSpoolBytes     int64 = 2 << 30 // 2 GiB bounded disk spool per session
	defaultMaxHybridAggregateBytes int64 = 4 << 30 // 4 GiB aggregate spool budget per gateway instance
	maxHybridStartupWait                 = 25 * time.Second
	maxHybridConversionDuration          = 10 * time.Minute
	defaultMaxConcurrentHybrid           = 2
)

// hybridSpool tracks bounded on-disk conversion for HYBRID and selected finite
// YouTube REMUX sessions that require a known-length response.
// Android AVAPIMediaPlayer on legacy Google TV (API 13) requires a definitive
// Content-Length header and rejects HTTP chunked transfer encoding at end of
// headers before reading fragments. Disk spooling provides an exact byte count,
// persistent seekable byte-range access and bounded cleanup.
type hybridSpool struct {
	manager      *hybridSpoolManager
	mu           sync.Mutex
	once         sync.Once
	done         chan struct{}
	path         string
	fileInfo     os.FileInfo
	size         int64
	modTime      time.Time
	err          error
	cleaned      bool
	readers      int
	lastActivity time.Time
	reserved     int64
	actualBytes  int64
}

func (sp *hybridSpool) acquireReader() error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cleaned || sp.path == "" {
		return errors.New("spool_unavailable")
	}
	sp.readers++
	sp.lastActivity = time.Now()
	return nil
}

func (sp *hybridSpool) releaseReader() {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.readers > 0 {
		sp.readers--
	}
	sp.lastActivity = time.Now()
}

func (sp *hybridSpool) readerInfo() (string, time.Time, error) {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cleaned || sp.path == "" {
		return "", time.Time{}, errors.New("spool_unavailable")
	}
	return sp.path, sp.modTime, nil
}

func (sp *hybridSpool) filePath() string {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return sp.path
}

func (sp *hybridSpool) cleanup() {
	if sp.manager != nil {
		sp.manager.cleanupSpool(sp)
		return
	}
	sp.mu.Lock()
	if sp.cleaned {
		sp.mu.Unlock()
		return
	}
	sp.cleaned = true
	path := sp.path
	sp.path = ""
	sp.mu.Unlock()
	if path != "" {
		_ = os.Remove(path)
	}
}

type boundedSpoolWriter struct {
	w     io.Writer
	limit int64
	total int64
}

func (b *boundedSpoolWriter) Write(p []byte) (int, error) {
	if b.total+int64(len(p)) > b.limit {
		return 0, ErrSpoolLimitExceeded
	}
	n, err := b.w.Write(p)
	b.total += int64(n)
	return n, err
}

type hybridSpoolManager struct {
	mu             sync.Mutex
	baseDir        string
	baseInfo       os.FileInfo
	instanceDir    string
	instanceInfo   os.FileInfo
	instanceFile   *os.File
	lockFile       *os.File
	lockInfo       os.FileInfo
	initErr        error
	maxSpoolBytes  int64
	aggregateQuota int64
	activeConvs    int
	maxConvs       int
	spools         map[*hybridSpool]struct{}
	workerContext  context.Context
	workerCancel   context.CancelFunc
	workerGroup    sync.WaitGroup
	closeDone      chan struct{}
	closed         bool
}

func defaultHybridSpoolDir() string {
	return filepath.Join(os.TempDir(), "zombiebox-spools")
}

func validateAndPrepareBaseDir(configuredDir string) (string, error) {
	raw := configuredDir
	if raw == "" {
		raw = defaultHybridSpoolDir()
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		abs, err := filepath.Abs(clean)
		if err != nil {
			return "", fmt.Errorf("%w: invalid base dir: %v", ErrSpoolStorageUnavailable, err)
		}
		clean = abs
	}

	currentUID := uint32(os.Getuid())

	fi, err := os.Lstat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(clean, 0700); err != nil {
				return "", fmt.Errorf("%w: cannot create base dir: %v", ErrSpoolStorageUnavailable, err)
			}
			fi, err = os.Lstat(clean)
			if err != nil {
				return "", fmt.Errorf("%w: cannot stat base dir: %v", ErrSpoolStorageUnavailable, err)
			}
		} else {
			return "", fmt.Errorf("%w: cannot stat base dir: %v", ErrSpoolStorageUnavailable, err)
		}
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: base dir is a symlink: %s", ErrSpoolStorageUnavailable, clean)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%w: base dir is not a directory: %s", ErrSpoolStorageUnavailable, clean)
	}

	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("%w: cannot determine file ownership", ErrSpoolStorageUnavailable)
	}

	// Never chmod an existing configured base directory.
	// If the existing directory is owned by the current user and has restrictive permissions (no group/other write access),
	// use it directly without mutating it.
	// If it is shared or owned by someone else (e.g. /tmp), create and use a private subdirectory inside it.
	isOwned := (stat.Uid == currentUID)
	isGroupOrWorldWritable := (fi.Mode().Perm()&0022 != 0)

	if isOwned && !isGroupOrWorldWritable {
		return clean, nil
	}

	privateDir := filepath.Join(clean, fmt.Sprintf("zombiebox-spools-%d", currentUID))
	pfi, err := os.Lstat(privateDir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.Mkdir(privateDir, 0700); err != nil {
				return "", fmt.Errorf("%w: cannot create private root: %v", ErrSpoolStorageUnavailable, err)
			}
			pfi, err = os.Lstat(privateDir)
			if err != nil {
				return "", fmt.Errorf("%w: cannot stat private root: %v", ErrSpoolStorageUnavailable, err)
			}
		} else {
			return "", fmt.Errorf("%w: cannot stat private root: %v", ErrSpoolStorageUnavailable, err)
		}
	}

	if pfi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: private root is a symlink: %s", ErrSpoolStorageUnavailable, privateDir)
	}
	if !pfi.IsDir() {
		return "", fmt.Errorf("%w: private root is not a directory: %s", ErrSpoolStorageUnavailable, privateDir)
	}
	pstat, ok := pfi.Sys().(*syscall.Stat_t)
	if !ok || pstat.Uid != currentUID {
		return "", fmt.Errorf("%w: private root not owned by current user", ErrSpoolStorageUnavailable)
	}
	if pfi.Mode().Perm()&0022 != 0 {
		return "", fmt.Errorf("%w: private root has insecure permissions", ErrSpoolStorageUnavailable)
	}

	return privateDir, nil
}

func ownedDirectory(path string, expected os.FileInfo, private bool) (os.FileInfo, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		return nil, false
	}
	if private && (info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0700 != 0700) {
		return nil, false
	}
	if expected != nil && !os.SameFile(expected, info) {
		return nil, false
	}
	return info, true
}

func ownedRegularFile(path string, expected os.FileInfo, opened *os.File) (os.FileInfo, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 || info.Mode().Perm()&0600 != 0600 {
		return nil, false
	}
	if expected != nil && !os.SameFile(expected, info) {
		return nil, false
	}
	if opened != nil {
		openedInfo, err := opened.Stat()
		if err != nil || !os.SameFile(info, openedInfo) {
			return nil, false
		}
	}
	return info, true
}

// cleanStaleHybridSpools cleans up instance directories abandoned by previous crashed processes.
// Guarantees:
//  1. Only removes an orphan when a valid regular file .lock owned by the current user can be opened safely (O_NOFOLLOW)
//     and an exclusive non-blocking advisory file lock (syscall.Flock) can be acquired.
//  2. Active instances are preserved because their live process holds LOCK_EX.
//  3. Never deletes a directory merely because .lock is missing, unreadable, or based on time elapsed.
//  4. Never follows symlinks.
func cleanStaleHybridSpools(baseDir ...string) {
	dir := defaultHybridSpoolDir()
	if len(baseDir) > 0 && baseDir[0] != "" {
		dir = baseDir[0]
	}
	dfi, ok := ownedDirectory(dir, nil, false)
	if !ok {
		return
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "instance-") {
			continue
		}
		dirPath := filepath.Join(dir, name)

		dirFi, ok := ownedDirectory(dirPath, nil, true)
		if !ok {
			continue
		}

		lockPath := filepath.Join(dirPath, ".lock")
		lockFi, ok := ownedRegularFile(lockPath, nil, nil)
		if !ok {
			// .lock missing, unreadable, or invalid: do NOT delete directory.
			continue
		}

		fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if err != nil {
			continue
		}
		lockFile := os.NewFile(uintptr(fd), lockPath)

		flockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if flockErr != nil {
			// Lock is held by an active live process: preserve intact!
			_ = lockFile.Close()
			continue
		}

		// Successfully locked: previous owner crashed or terminated without cleaning up.
		_, parentStillOwned := ownedDirectory(dir, dfi, false)
		_, instanceStillOwned := ownedDirectory(dirPath, dirFi, true)
		_, lockStillOwned := ownedRegularFile(lockPath, lockFi, lockFile)
		if parentStillOwned && instanceStillOwned && lockStillOwned {
			_ = os.RemoveAll(dirPath)
		}
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}
}

func newHybridSpoolManager(opt Options) *hybridSpoolManager {
	maxSpool := opt.HybridMaxSpoolBytes
	if maxSpool <= 0 {
		maxSpool = defaultMaxHybridSpoolBytes
	}
	aggQuota := opt.HybridAggregateQuotaBytes
	if aggQuota <= 0 {
		aggQuota = defaultMaxHybridAggregateBytes
	}
	maxConvs := opt.HybridMaxConversions
	if maxConvs <= 0 {
		maxConvs = defaultMaxConcurrentHybrid
	}

	m := &hybridSpoolManager{
		maxSpoolBytes:  maxSpool,
		aggregateQuota: aggQuota,
		maxConvs:       maxConvs,
		spools:         make(map[*hybridSpool]struct{}),
		closeDone:      make(chan struct{}),
	}
	m.workerContext, m.workerCancel = context.WithCancel(context.Background())

	baseDir, err := validateAndPrepareBaseDir(opt.HybridSpoolDir)
	if err != nil {
		m.initErr = err
		return m
	}
	m.baseDir = baseDir

	cleanStaleHybridSpools(baseDir)
	baseInfo, ok := ownedDirectory(baseDir, nil, false)
	if !ok {
		m.initErr = fmt.Errorf("%w: base dir ownership or permissions changed", ErrSpoolStorageUnavailable)
		return m
	}
	m.baseInfo = baseInfo

	instanceDir := filepath.Join(baseDir, fmt.Sprintf("instance-%d-%s", os.Getpid(), randomID(8)))
	if err := os.Mkdir(instanceDir, 0700); err != nil {
		m.initErr = fmt.Errorf("%w: failed to create instance dir: %v", ErrSpoolStorageUnavailable, err)
		return m
	}

	instFi, ok := ownedDirectory(instanceDir, nil, true)
	if !ok {
		m.initErr = fmt.Errorf("%w: instance dir validation failed", ErrSpoolStorageUnavailable)
		return m
	}
	instanceFile, err := os.Open(instanceDir)
	if err != nil {
		if _, stillOwned := ownedDirectory(instanceDir, instFi, true); stillOwned {
			_ = os.Remove(instanceDir)
		}
		m.initErr = fmt.Errorf("%w: failed to open instance dir: %v", ErrSpoolStorageUnavailable, err)
		return m
	}
	openedInstance, err := instanceFile.Stat()
	if err != nil || !os.SameFile(instFi, openedInstance) {
		_ = instanceFile.Close()
		m.initErr = fmt.Errorf("%w: instance dir changed while opening", ErrSpoolStorageUnavailable)
		return m
	}

	lockPath := filepath.Join(instanceDir, ".lock")
	fd, err := syscall.Open(lockPath, syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		_ = instanceFile.Close()
		if _, stillOwned := ownedDirectory(instanceDir, instFi, true); stillOwned {
			_ = os.Remove(instanceDir)
		}
		m.initErr = fmt.Errorf("%w: failed to create lock file: %v", ErrSpoolStorageUnavailable, err)
		return m
	}
	lockFile := os.NewFile(uintptr(fd), lockPath)
	lockInfo, ok := ownedRegularFile(lockPath, nil, lockFile)
	if !ok {
		_ = lockFile.Close()
		_ = instanceFile.Close()
		m.initErr = fmt.Errorf("%w: lock file validation failed", ErrSpoolStorageUnavailable)
		return m
	}

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_, lockStillOwned := ownedRegularFile(lockPath, lockInfo, lockFile)
		if lockStillOwned {
			_ = syscall.Unlinkat(int(instanceFile.Fd()), ".lock")
		}
		_ = lockFile.Close()
		_ = instanceFile.Close()
		if _, instanceStillOwned := ownedDirectory(instanceDir, instFi, true); instanceStillOwned {
			_ = os.Remove(instanceDir)
		}
		m.initErr = fmt.Errorf("%w: failed to acquire flock on instance lock: %v", ErrSpoolStorageUnavailable, err)
		return m
	}

	m.instanceDir = instanceDir
	m.instanceInfo = instFi
	m.instanceFile = instanceFile
	m.lockFile = lockFile
	m.lockInfo = lockInfo
	return m
}

func (m *hybridSpoolManager) Close() {
	m.mu.Lock()
	if m.closed {
		done := m.closeDone
		m.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	m.closed = true
	m.mu.Unlock()

	if m.workerCancel != nil {
		m.workerCancel()
	}
	// Admission and WaitGroup.Add share m.mu, so no worker can register after
	// closed is set. Do not hold m.mu here: workers need it to finish cleanup.
	m.workerGroup.Wait()

	m.mu.Lock()
	spoolsToClean := make([]*hybridSpool, 0, len(m.spools))
	for sp := range m.spools {
		spoolsToClean = append(spoolsToClean, sp)
	}
	m.mu.Unlock()
	for _, sp := range spoolsToClean {
		m.cleanupSpool(sp)
	}

	m.mu.Lock()
	lockFile := m.lockFile
	instanceDir := m.instanceDir
	instanceFile := m.instanceFile
	pathsTrusted := m.pathsTrustedLocked()
	if pathsTrusted && instanceFile != nil {
		_ = syscall.Unlinkat(int(instanceFile.Fd()), ".lock")
	}
	m.lockFile = nil
	m.lockInfo = nil
	m.mu.Unlock()

	if lockFile != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}
	if instanceDir != "" && pathsTrusted {
		m.mu.Lock()
		stillSameInstance := m.parentAndInstanceTrustedLocked()
		m.mu.Unlock()
		if stillSameInstance {
			// Remove only an empty directory. Never recursively delete a path that
			// may have been replaced while shutdown was waiting for workers.
			_ = os.Remove(instanceDir)
		}
	}
	if instanceFile != nil {
		_ = instanceFile.Close()
	}
	m.mu.Lock()
	if m.closeDone != nil {
		close(m.closeDone)
	}
	m.mu.Unlock()
}

// parentAndInstanceTrustedLocked verifies that the path names still refer to
// the directories this manager created. Caller must hold m.mu.
func (m *hybridSpoolManager) parentAndInstanceTrustedLocked() bool {
	if m.baseInfo == nil || m.instanceInfo == nil || m.instanceFile == nil {
		return false
	}
	if _, ok := ownedDirectory(m.baseDir, m.baseInfo, false); !ok {
		return false
	}
	if _, ok := ownedDirectory(m.instanceDir, m.instanceInfo, true); !ok {
		return false
	}
	opened, err := m.instanceFile.Stat()
	return err == nil && os.SameFile(m.instanceInfo, opened)
}

// pathsTrustedLocked additionally verifies that .lock still names the locked
// file. Caller must hold m.mu.
func (m *hybridSpoolManager) pathsTrustedLocked() bool {
	if !m.parentAndInstanceTrustedLocked() || m.lockFile == nil || m.lockInfo == nil {
		return false
	}
	lockPath := filepath.Join(m.instanceDir, ".lock")
	_, ok := ownedRegularFile(lockPath, m.lockInfo, m.lockFile)
	return ok
}

func (m *hybridSpoolManager) createSpoolTemp() (*os.File, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed || m.initErr != nil || !m.pathsTrustedLocked() || m.instanceFile == nil {
		return nil, "", ErrSpoolStorageUnavailable
	}

	for attempt := 0; attempt < 8; attempt++ {
		name := "spool-" + randomID(12) + ".mp4"
		fd, err := syscall.Openat(
			int(m.instanceFile.Fd()), name,
			syscall.O_RDWR|syscall.O_CREAT|syscall.O_EXCL|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
			0600,
		)
		if err == syscall.EEXIST {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create spool temp: %w", err)
		}
		if !m.pathsTrustedLocked() {
			_ = syscall.Unlinkat(int(m.instanceFile.Fd()), name)
			_ = syscall.Close(fd)
			return nil, "", ErrSpoolStorageUnavailable
		}
		path := filepath.Join(m.instanceDir, name)
		return os.NewFile(uintptr(fd), path), path, nil
	}
	return nil, "", errors.New("create spool temp: name collision limit reached")
}

// removeSpoolFileLocked removes only a direct child with the identity captured
// from the opened spool file. Caller must hold m.mu.
func (m *hybridSpoolManager) removeSpoolFileLocked(path string, expected os.FileInfo) {
	if m.instanceFile == nil || filepath.Dir(filepath.Clean(path)) != filepath.Clean(m.instanceDir) {
		return
	}
	name := filepath.Base(path)
	if !strings.HasPrefix(name, "spool-") || strings.Contains(name, string(filepath.Separator)) {
		return
	}
	fd, err := syscall.Openat(int(m.instanceFile.Fd()), name, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	_ = file.Close()
	if err != nil || !info.Mode().IsRegular() || (expected != nil && !os.SameFile(expected, info)) {
		return
	}
	_ = syscall.Unlinkat(int(m.instanceFile.Fd()), name)
}

func (m *hybridSpoolManager) removeSpoolFile(path string, expected os.FileInfo) {
	m.mu.Lock()
	m.removeSpoolFileLocked(path, expected)
	m.mu.Unlock()
}

func (m *hybridSpoolManager) maxSpoolLimit() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxSpoolBytes
}

// currentUsageLocked calculates total accounted bytes.
// Caller must hold m.mu.
// Consistent lock order: m.mu -> sp.mu.
func (m *hybridSpoolManager) currentUsageLocked() int64 {
	var total int64
	for sp := range m.spools {
		sp.mu.Lock()
		if !sp.cleaned {
			if sp.reserved > 0 {
				total += sp.reserved
			} else {
				total += sp.actualBytes
			}
		}
		sp.mu.Unlock()
	}
	return total
}

// evictLocked evicts completed, idle spools in LRU order to make room for needed bytes.
// Caller must hold m.mu.
// Consistent lock order: m.mu -> sp.mu.
func (m *hybridSpoolManager) evictLocked(needed int64) {
	type candidate struct {
		sp       *hybridSpool
		activity time.Time
	}
	var candidates []candidate
	for sp := range m.spools {
		sp.mu.Lock()
		if !sp.cleaned && sp.reserved == 0 && sp.readers == 0 {
			candidates = append(candidates, candidate{sp: sp, activity: sp.lastActivity})
		}
		sp.mu.Unlock()
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].activity.Before(candidates[j].activity)
	})

	var evictedSpools []*hybridSpool
	for _, c := range candidates {
		if m.currentUsageLocked()+needed <= m.aggregateQuota {
			break
		}
		delete(m.spools, c.sp)
		evictedSpools = append(evictedSpools, c.sp)
	}

	for _, sp := range evictedSpools {
		sp.mu.Lock()
		sp.cleaned = true
		if sp.reserved > 0 {
			m.activeConvs--
			sp.reserved = 0
		}
		sp.actualBytes = 0
		path := sp.path
		fileInfo := sp.fileInfo
		sp.path = ""
		sp.fileInfo = nil
		sp.mu.Unlock()
		if path != "" {
			m.removeSpoolFileLocked(path, fileInfo)
		}
	}
}

func (m *hybridSpoolManager) acquireSpool(sess *session, start func(context.Context, *session)) (*hybridSpool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.initErr != nil {
		sp := &hybridSpool{manager: m, done: make(chan struct{}), err: m.initErr}
		close(sp.done)
		return sp, sp.err
	}

	if m.closed {
		sp := &hybridSpool{manager: m, done: make(chan struct{}), err: errors.New("spool_closed")}
		close(sp.done)
		return sp, sp.err
	}

	if m.activeConvs >= m.maxConvs {
		sp := &hybridSpool{manager: m, done: make(chan struct{}), err: media.ErrBusy}
		close(sp.done)
		return sp, sp.err
	}

	needed := m.maxSpoolBytes
	if m.currentUsageLocked()+needed > m.aggregateQuota {
		m.evictLocked(needed)
	}

	if m.currentUsageLocked()+needed > m.aggregateQuota {
		sp := &hybridSpool{manager: m, done: make(chan struct{}), err: ErrSpoolQuotaExceeded}
		close(sp.done)
		return sp, sp.err
	}

	m.activeConvs++
	spool := &hybridSpool{
		manager:      m,
		done:         make(chan struct{}),
		reserved:     needed,
		lastActivity: time.Now(),
	}
	m.spools[spool] = struct{}{}
	workerCtx, workerCancel := context.WithCancel(m.workerContext)
	stopSessionCancel := context.AfterFunc(sess.ctx, workerCancel)
	m.workerGroup.Add(1)
	sess.hybridSpool = spool
	go func() {
		defer m.workerGroup.Done()
		defer stopSessionCancel()
		defer workerCancel()
		start(workerCtx, sess)
	}()

	context.AfterFunc(sess.ctx, func() {
		spool.cleanup()
	})

	return spool, nil
}

// onSpoolCompleted transitions an active spool from upfront reservation to exact byte size.
// Respects m.mu -> sp.mu lock ordering.
func (m *hybridSpoolManager) onSpoolCompleted(sp *hybridSpool, size int64, modTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.spools[sp]; !ok {
		return
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.cleaned {
		return
	}
	if sp.reserved > 0 {
		m.activeConvs--
		sp.reserved = 0
	}
	sp.actualBytes = size
	sp.size = size
	sp.modTime = modTime
	sp.lastActivity = time.Now()
}

// cleanupSpool safely cleans and deletes a spool.
// Respects m.mu -> sp.mu lock ordering.
func (m *hybridSpoolManager) cleanupSpool(sp *hybridSpool) {
	m.mu.Lock()
	sp.mu.Lock()

	if sp.cleaned {
		sp.mu.Unlock()
		m.mu.Unlock()
		return
	}
	sp.cleaned = true
	delete(m.spools, sp)
	if sp.reserved > 0 {
		m.activeConvs--
		sp.reserved = 0
	}
	sp.actualBytes = 0
	path := sp.path
	fileInfo := sp.fileInfo
	sp.path = ""
	sp.fileInfo = nil
	if path != "" {
		m.removeSpoolFileLocked(path, fileInfo)
	}
	sp.mu.Unlock()
	m.mu.Unlock()
}

func (m *hybridSpoolManager) onSpoolCleaned(sp *hybridSpool) {
	m.cleanupSpool(sp)
}

func (s *Server) newHybridSpool(sess *session) *hybridSpool {
	if s.hybridSpools == nil {
		s.hybridSpools = newHybridSpoolManager(s.opt)
	}
	sp, _ := s.hybridSpools.acquireSpool(sess, func(ctx context.Context, sess *session) {
		s.runHybridSpool(ctx, sess)
	})
	return sp
}

func (s *Server) runHybridSpool(parentCtx context.Context, sess *session) {
	spool := sess.hybridSpool
	if spool == nil {
		return
	}
	spool.once.Do(func() {
		defer close(spool.done)
		manager := spool.manager

		if (sess.source.Path != "" && s.deps.Media == nil) || (sess.source.Path == "" && s.deps.RemoteMedia == nil) {
			spool.mu.Lock()
			spool.err = errors.New("conversion_unavailable")
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		if manager == nil {
			spool.mu.Lock()
			spool.err = ErrSpoolStorageUnavailable
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		spool.mu.Lock()
		if spool.cleaned {
			spool.mu.Unlock()
			return
		}
		spool.mu.Unlock()

		if err := parentCtx.Err(); err != nil {
			spool.mu.Lock()
			spool.err = err
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		tmpFile, spoolPath, err := manager.createSpoolTemp()
		if err != nil {
			spool.mu.Lock()
			spool.err = err
			spool.mu.Unlock()
			spool.cleanup()
			return
		}
		fileInfo, err := tmpFile.Stat()
		if err != nil {
			_ = tmpFile.Close()
			manager.removeSpoolFile(spoolPath, nil)
			spool.mu.Lock()
			spool.err = err
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		spool.mu.Lock()
		if spool.cleaned {
			spool.mu.Unlock()
			_ = tmpFile.Close()
			manager.removeSpoolFile(spoolPath, fileInfo)
			return
		}
		spool.path = spoolPath
		spool.fileInfo = fileInfo
		spool.mu.Unlock()

		ctx, cancel := context.WithTimeout(parentCtx, maxHybridConversionDuration)
		defer cancel()

		writer := &boundedSpoolWriter{
			w:     tmpFile,
			limit: manager.maxSpoolLimit(),
		}

		if sess.source.Path != "" {
			err = s.deps.Media.ConvertSelected(ctx, sess.source.Path, sess.mode, sess.selection, writer)
		} else {
			err = s.deps.RemoteMedia.ConvertRemote(ctx, sess.source, sess.mode, sess.selection, writer)
		}
		if err == nil {
			err = ctx.Err()
		}

		closeErr := tmpFile.Close()

		spool.mu.Lock()
		if spool.cleaned {
			spool.mu.Unlock()
			manager.removeSpoolFile(spoolPath, fileInfo)
			return
		}
		spool.mu.Unlock()

		if err != nil || closeErr != nil {
			if err == nil {
				err = closeErr
			}
			spool.mu.Lock()
			spool.err = err
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		info, err := os.Lstat(spoolPath)
		if err != nil {
			spool.mu.Lock()
			spool.err = err
			spool.mu.Unlock()
			spool.cleanup()
			return
		}
		if info.Mode()&os.ModeSymlink != 0 || !os.SameFile(fileInfo, info) {
			spool.mu.Lock()
			spool.err = ErrSpoolStorageUnavailable
			spool.mu.Unlock()
			spool.cleanup()
			return
		}
		if info.Size() == 0 {
			spool.mu.Lock()
			spool.err = errors.New("spool_empty")
			spool.mu.Unlock()
			spool.cleanup()
			return
		}

		manager.onSpoolCompleted(spool, info.Size(), info.ModTime())
	})
}

func (s *Server) getOrStartHybridSpool(sess *session) *hybridSpool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sess.hybridSpool != nil {
		sess.hybridSpool.mu.Lock()
		err := sess.hybridSpool.err
		sess.hybridSpool.mu.Unlock()
		if errors.Is(err, media.ErrBusy) || errors.Is(err, ErrSpoolQuotaExceeded) {
			sess.hybridSpool = nil
		}
	}

	if sess.hybridSpool == nil {
		if s.hybridSpools == nil {
			s.hybridSpools = newHybridSpoolManager(s.opt)
		}
		spool, err := s.hybridSpools.acquireSpool(sess, func(ctx context.Context, sess *session) {
			s.runHybridSpool(ctx, sess)
		})
		if err == nil {
			return sess.hybridSpool
		} else {
			// Do not permanently cache transient allocation failure on session
			return spool
		}
	}
	return sess.hybridSpool
}
