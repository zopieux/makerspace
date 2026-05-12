package usb

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"

	ublk "github.com/semistrict/go-ublk"
)

// ------------------------------------------------------------
// FAT32 geometry constants (all sizes in sectors of 512 bytes)
// ------------------------------------------------------------
const (
	sectorSize        = 512
	sectorsPerCluster = 8  // 4 KiB clusters — good for up to ~2 GB
	reservedSectors   = 32 // includes boot + FSInfo + padding
	numFATs           = 2
	rootCluster       = 2 // FAT32: root dir starts at cluster 2
	fsInfoSector      = 1

	// FAT32 special values
	fatEOC  = uint32(0x0FFFFFF8)
	fatFree = uint32(0x00000000)
	fatBad  = uint32(0x0FFFFFF7)
)

// ------------------------------------------------------------
// Build-time representation of a file/directory node
// ------------------------------------------------------------
type node struct {
	name         string // original name
	realPath     string // absolute path on host (empty for dirs)
	isDir        bool
	size         uint32
	children     []*node
	startCluster uint32
	modTime      time.Time
}

// ------------------------------------------------------------
// VirtualFAT — the core structure
// ------------------------------------------------------------
type VirtualFAT struct {
	root          *node
	label         string
	fat           []uint32 // cluster index → next cluster (or EOC)
	totalClusters uint32
	totalSectors  uint64

	// pre-rendered byte regions
	bootSector [sectorSize]byte
	fsInfo     [sectorSize]byte
	fatBytes   []byte // both FATs concatenated
	dirRegion  []byte // all directory sectors, indexed by cluster

	// cluster → node (for data reads)
	clusterToNode map[uint32]*node
	// cluster → directory sector data (for dir reads)
	clusterToDirData map[uint32][]byte

	fatStartSector  uint64
	dataStartSector uint64

	sourceDir    string
	DeleteEvents chan string
}

// ------------------------------------------------------------
// Entry point: build the virtual filesystem from a directory
// ------------------------------------------------------------
func NewVirtualFAT(root string, label string) (*VirtualFAT, error) {
	if len(label) > 11 {
		label = label[:11]
	}
	v := &VirtualFAT{
		clusterToNode:    make(map[uint32]*node),
		clusterToDirData: make(map[uint32][]byte),
		label:            fmt.Sprintf("%-11s", strings.ToUpper(label)),
		sourceDir:        root,
		DeleteEvents:     make(chan string, 100),
	}

	// 1. Walk the source directory and build the node tree
	rootNode, err := v.walk(root)
	if err != nil {
		return nil, fmt.Errorf("walk: %w", err)
	}
	v.root = rootNode

	// 2. Assign clusters (BFS order, root dir gets cluster 2)
	nextCluster := uint32(rootCluster)
	v.assignClusters(rootNode, &nextCluster)

	if nextCluster < 65538 {
		nextCluster = 65538
	}
	v.totalClusters = nextCluster // clusters 0..nextCluster-1 are used

	// FAT sectors needed: ceil(totalClusters * 4 / sectorSize)
	fatSectorCount := (v.totalClusters*4 + sectorSize - 1) / sectorSize

	v.fatStartSector = uint64(reservedSectors)
	v.dataStartSector = v.fatStartSector + uint64(numFATs)*uint64(fatSectorCount)

	totalDataClusters := v.totalClusters - rootCluster
	totalDataSectors := uint64(totalDataClusters) * sectorsPerCluster
	v.totalSectors = v.dataStartSector + totalDataSectors

	// 3. Build FAT table
	v.fat = make([]uint32, v.totalClusters)
	v.fat[0] = 0x0FFFFFF8 // media byte
	v.fat[1] = 0x0FFFFFFF // reserved
	v.buildFATChains(rootNode)

	v.fatBytes = make([]byte, uint64(numFATs)*uint64(fatSectorCount)*sectorSize)
	for i, val := range v.fat {
		binary.LittleEndian.PutUint32(v.fatBytes[i*4:], val)
		// FAT2 copy
		offset := uint64(fatSectorCount) * sectorSize
		binary.LittleEndian.PutUint32(v.fatBytes[uint64(i*4)+offset:], val)
	}

	// 4. Build directory sectors
	v.buildDirSectors(rootNode)

	// 5. Build boot sector and FSInfo
	v.buildBootSector(fatSectorCount)
	v.buildFSInfo()

	return v, nil
}

// makeVolumeLabelEntry makes a FAT12 volume label directory entry.
func makeVolumeLabelEntry(label string, t time.Time) []byte {
	e := make([]byte, 32)
	// 8.3 field used as full 11-char label, space-padded
	copy(e[0:11], []byte(label))
	e[11] = 0x08 // Volume label attribute
	date, tim := fatTime(t)
	binary.LittleEndian.PutUint16(e[22:], tim)
	binary.LittleEndian.PutUint16(e[24:], date)
	// cluster = 0 for volume label entry
	return e
}

// ------------------------------------------------------------
// Walk: build node tree
// ------------------------------------------------------------
func (v *VirtualFAT) walk(path string) (*node, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	n := &node{
		name:    info.Name(),
		modTime: info.ModTime(),
		isDir:   info.IsDir(),
	}
	if !info.IsDir() {
		n.realPath = path
		n.size = uint32(info.Size())
		return n, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		child, err := v.walk(filepath.Join(path, e.Name()))
		if err != nil {
			log.Printf("skipping %s: %v", e.Name(), err)
			continue
		}
		n.children = append(n.children, child)
	}
	return n, nil
}

// ------------------------------------------------------------
// Assign clusters: every file/dir gets ceil(size/clusterBytes) clusters
// ------------------------------------------------------------
func (v *VirtualFAT) assignClusters(n *node, next *uint32) {
	if n.isDir {
		n.startCluster = *next
		// directories always get at least 1 cluster
		dirSize := v.dirSectorCount(n) * sectorSize
		clusters := (dirSize + sectorsPerCluster*sectorSize - 1) / (sectorsPerCluster * sectorSize)
		if clusters == 0 {
			clusters = 1
		}
		*next += uint32(clusters)
		for _, child := range n.children {
			v.assignClusters(child, next)
		}
	} else {
		if n.size == 0 {
			n.startCluster = 0
			return
		}
		n.startCluster = *next
		clusters := (n.size + sectorsPerCluster*sectorSize - 1) / (sectorsPerCluster * sectorSize)
		*next += clusters
		v.clusterToNode[n.startCluster] = n
		// record all clusters of this file
		for i := uint32(1); i < clusters; i++ {
			v.clusterToNode[n.startCluster+i] = n
		}
	}
}

// ------------------------------------------------------------
// Build FAT chains
// ------------------------------------------------------------
func (v *VirtualFAT) buildFATChains(n *node) {
	if !n.isDir && n.size == 0 {
		return
	}
	var clusters uint32
	if n.isDir {
		dirSize := v.dirSectorCount(n) * sectorSize
		clusters = (uint32(dirSize) + sectorsPerCluster*sectorSize - 1) / (sectorsPerCluster * sectorSize)
		if clusters == 0 {
			clusters = 1
		}
	} else {
		clusters = (n.size + sectorsPerCluster*sectorSize - 1) / (sectorsPerCluster * sectorSize)
	}
	for i := uint32(0); i < clusters-1; i++ {
		v.fat[n.startCluster+i] = n.startCluster + i + 1
	}
	v.fat[n.startCluster+clusters-1] = fatEOC

	if n.isDir {
		for _, child := range n.children {
			v.buildFATChains(child)
		}
	}
}

// ------------------------------------------------------------
// Directory entry building
// ------------------------------------------------------------

// Number of directory sectors needed for a node's children
func (v *VirtualFAT) dirSectorCount(n *node) uint32 {
	// Each child may need LFN entries + 1 short entry; root also has . and ..
	entries := 0
	if n != v.root {
		entries += 2 // Only add . and .. for subdirs
	}
	for _, child := range n.children {
		entries += lfnEntryCount(child.name) + 1
	}
	bytesNeeded := entries * 32
	sectors := (bytesNeeded + sectorSize - 1) / sectorSize
	if sectors == 0 {
		sectors = 1
	}
	return uint32(sectors)
}

func (v *VirtualFAT) buildDirSectors(n *node) {
	var entries []byte

	if n == v.root {
		entries = append(entries, makeVolumeLabelEntry(v.label, n.modTime)...)
	} else {
		dot := makeDotEntry(".", n.startCluster, n.modTime)
		dotdot := makeDotEntry("..", 0, n.modTime) // 0 = root for simplicity
		entries = append(entries, dot...)
		entries = append(entries, dotdot...)
	}

	for _, child := range n.children {
		entries = append(entries, makeDirEntries(child)...)
	}

	// Pad to cluster boundary
	clusterBytes := sectorsPerCluster * sectorSize
	padded := ((len(entries) + clusterBytes - 1) / clusterBytes) * clusterBytes
	if padded == 0 {
		padded = clusterBytes
	}
	buf := make([]byte, padded)
	copy(buf, entries)

	v.clusterToDirData[n.startCluster] = buf

	// Recurse
	for _, child := range n.children {
		if child.isDir {
			v.buildDirSectors(child)
		}
	}
}

// ------------------------------------------------------------
// Boot sector (BPB)
// ------------------------------------------------------------
func (v *VirtualFAT) buildBootSector(fatSectors uint32) {
	b := v.bootSector[:]
	// Jump + OEM
	b[0], b[1], b[2] = 0xEB, 0x58, 0x90
	copy(b[3:11], "MSDOS5.0")

	put16 := func(off int, val uint16) { binary.LittleEndian.PutUint16(b[off:], val) }
	put32 := func(off int, val uint32) { binary.LittleEndian.PutUint32(b[off:], val) }

	put16(11, sectorSize)             // bytes per sector
	b[13] = sectorsPerCluster         // sectors per cluster
	put16(14, reservedSectors)        // reserved sectors
	b[16] = numFATs                   // number of FATs
	put16(17, 0)                      // root entry count (0 for FAT32)
	put16(19, 0)                      // total sectors 16 (0 = use 32-bit field)
	b[21] = 0xF8                      // media type
	put16(22, 0)                      // FAT size 16 (0 for FAT32)
	put16(24, 32)                     // sectors per track
	put16(26, 2)                      // number of heads
	put32(28, 0)                      // hidden sectors
	put32(32, uint32(v.totalSectors)) // total sectors 32
	put32(36, fatSectors)             // FAT size 32
	put16(40, 0)                      // ext flags
	put16(42, 0)                      // FS version
	put32(44, rootCluster)            // root cluster
	put16(48, fsInfoSector)           // FSInfo sector
	put16(50, 6)                      // backup boot sector
	b[64] = 0x80                      // drive number
	b[66] = 0x29                      // extended boot signature
	put32(67, 0xDEADBEEF)             // volume serial
	copy(b[71:82], v.label)           // volume label
	copy(b[82:90], "FAT32   ")        // FS type
	b[510] = 0x55
	b[511] = 0xAA
}

func (v *VirtualFAT) buildFSInfo() {
	b := v.fsInfo[:]
	binary.LittleEndian.PutUint32(b[0:], 0x41615252)   // lead sig
	binary.LittleEndian.PutUint32(b[484:], 0x61417272) // struct sig
	binary.LittleEndian.PutUint32(b[488:], v.totalClusters-rootCluster-1) // Actual free count
	binary.LittleEndian.PutUint32(b[492:], 0xFFFFFFFF) // next free unknown
	binary.LittleEndian.PutUint32(b[508:], 0xAA550000) // trail sig
}

// ------------------------------------------------------------
// ReadAt — the heart of the virtual device
// ------------------------------------------------------------
func (v *VirtualFAT) ReadAt(buf []byte, off int64) (int, error) {
	end := uint64(off) + uint64(len(buf))
	_ = end

	// Fill buf byte-by-byte by sector dispatch.
	// In practice reads are sector-aligned and sector-sized, but handle general case.
	filled := 0
	for filled < len(buf) {
		sector := uint64(off+int64(filled)) / sectorSize
		sectorOff := int((uint64(off) + uint64(filled)) % sectorSize)
		canRead := sectorSize - sectorOff
		if canRead > len(buf)-filled {
			canRead = len(buf) - filled
		}

		sectorBuf, err := v.readSector(sector)
		if err != nil {
			return filled, err
		}
		copy(buf[filled:filled+canRead], sectorBuf[sectorOff:sectorOff+canRead])
		filled += canRead
	}
	return filled, nil
}

func (v *VirtualFAT) readSector(sector uint64) ([]byte, error) {
	zero := func() []byte { return make([]byte, sectorSize) }

	fatSectors := uint64(len(v.fatBytes)) / sectorSize

	switch {
	case sector == 0:
		return v.bootSector[:], nil

	case sector == uint64(fsInfoSector):
		return v.fsInfo[:], nil

	case sector < v.fatStartSector:
		// other reserved sectors
		return zero(), nil

	case sector < v.fatStartSector+fatSectors:
		rel := (sector - v.fatStartSector) * sectorSize
		return v.fatBytes[rel : rel+sectorSize], nil

	case sector >= v.dataStartSector:
		return v.readDataSector(sector)
	}
	return zero(), nil
}

func (v *VirtualFAT) readDataSector(sector uint64) ([]byte, error) {
	rel := sector - v.dataStartSector
	cluster := uint32(rel/sectorsPerCluster) + rootCluster
	sectorInCluster := rel % sectorsPerCluster

	// Is it a directory cluster?
	if data, ok := v.clusterToDirData[cluster]; ok {
		byteOff := (sectorInCluster) * sectorSize
		if int(byteOff)+sectorSize <= len(data) {
			return data[byteOff : byteOff+sectorSize], nil
		}
		return make([]byte, sectorSize), nil
	}

	// It's a file cluster
	n, ok := v.clusterToNode[cluster]
	if !ok {
		return make([]byte, sectorSize), nil
	}

	// Intercept: fire on first sector of the file
	clusterByteSize := uint32(sectorsPerCluster * sectorSize)
	clusterIndex := cluster - n.startCluster
	fileByteOffset := int64(clusterIndex)*int64(clusterByteSize) + int64(sectorInCluster)*sectorSize

	log.Printf("[ublk] read %s: %d", n.realPath, fileByteOffset)

	// Zero-copy: read directly from real file
	buf := make([]byte, sectorSize)
	if n.size == 0 || fileByteOffset >= int64(n.size) {
		return buf, nil
	}
	f, err := os.Open(n.realPath)
	if err != nil {
		return buf, nil
	}
	defer f.Close()
	n2, err := f.ReadAt(buf, fileByteOffset)
	if err != nil && err != io.EOF {
		return buf, nil
	}
	_ = n2
	return buf, nil
}

// Implement io.ReaderAt so we can use ublk.NewReaderAtHandler
func (v *VirtualFAT) ReadAt2(p []byte, off int64) (int, error) {
	return v.ReadAt(p, off)
}

type readerAtWrapper struct{ v *VirtualFAT }

func (r readerAtWrapper) ReadAt(p []byte, off int64) (int, error) {
	return r.v.ReadAt(p, off)
}

func (r readerAtWrapper) WriteAt(p []byte, off int64) (int, error) {
	return r.v.WriteAt(p, off)
}

func (v *VirtualFAT) WriteAt(buf []byte, off int64) (int, error) {
	startSector := uint64(off) / sectorSize

	for i := 0; i < len(buf); i += sectorSize {
		sector := startSector + uint64(i/sectorSize)
		end := i + sectorSize
		if end > len(buf) {
			end = len(buf)
		}
		chunk := buf[i:end]

		if sector >= v.fatStartSector && sector < v.dataStartSector {
			v.scanFATWrite(chunk, sector)
		} else if sector >= v.dataStartSector {
			v.scanDirWrite(chunk, sector)
		}
	}
	return len(buf), nil
}

func (v *VirtualFAT) scanFATWrite(chunk []byte, sector uint64) {
	return
	// Optional: track cluster frees (FAT rather than dir).
	for i := 0; i+4 <= len(chunk); i += 4 {
		val := binary.LittleEndian.Uint32(chunk[i:])
		cluster := uint32(sector-v.fatStartSector)*128 + uint32(i/4)
		if val == fatFree && cluster >= rootCluster && int(cluster) < len(v.fat) {
			if n, ok := v.clusterToNode[cluster]; ok {
				rel, _ := filepath.Rel(v.sourceDir, n.realPath)
				log.Printf("[intercept] cluster %d freed (file: %s)", cluster, rel)
			}
		}
	}
}

func (v *VirtualFAT) scanDirWrite(chunk []byte, sector uint64) {
	rel := sector - v.dataStartSector
	cluster := uint32(rel/sectorsPerCluster) + rootCluster
	if _, isDir := v.clusterToDirData[cluster]; !isDir {
		return
	}
	for i := 0; i+32 <= len(chunk); i += 32 {
		if chunk[i] == 0xE5 {
			hi := binary.LittleEndian.Uint16(chunk[i+20:])
			lo := binary.LittleEndian.Uint16(chunk[i+26:])
			fileCluster := uint32(hi)<<16 | uint32(lo)
			if n, ok := v.clusterToNode[fileCluster]; ok {
				relPath, _ := filepath.Rel(v.sourceDir, n.realPath)
				select {
				case v.DeleteEvents <- relPath:
				default:
				}
			}
		}
	}
}

// ------------------------------------------------------------
// FAT directory entry helpers
// ------------------------------------------------------------

func to83(name string) [11]byte {
	var out [11]byte
	for i := range out {
		out[i] = ' '
	}
	ext := ""
	base := name
	if idx := strings.LastIndex(name, "."); idx >= 0 && idx < len(name)-1 {
		base = name[:idx]
		ext = strings.ToUpper(name[idx+1:])
	}
	base = strings.ToUpper(base)
	for i := 0; i < 8 && i < len(base); i++ {
		out[i] = base[i]
	}
	for i := 0; i < 3 && i < len(ext); i++ {
		out[8+i] = ext[i]
	}
	return out
}

func lfnEntryCount(name string) int {
	// 13 UTF-16 chars per LFN entry
	return (len([]rune(name)) + 12) / 13
}

func fatTime(t time.Time) (uint16, uint16) {
	date := uint16(((t.Year()-1980)&0x7F)<<9 | int(t.Month())<<5 | t.Day())
	tim := uint16(t.Hour()<<11 | t.Minute()<<5 | t.Second()/2)
	return date, tim
}

func makeDotEntry(name string, cluster uint32, t time.Time) []byte {
	e := make([]byte, 32)
	if name == "." {
		copy(e[0:11], ".          ")
	} else {
		copy(e[0:11], "..         ")
	}
	e[11] = 0x10 // directory
	date, tim := fatTime(t)
	binary.LittleEndian.PutUint16(e[22:], tim)
	binary.LittleEndian.PutUint16(e[24:], date)
	binary.LittleEndian.PutUint16(e[20:], uint16(cluster>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(cluster&0xFFFF))
	return e
}

func makeDirEntries(n *node) []byte {
	var out []byte

	// LFN entries (in reverse order, as per FAT spec)
	runes := []rune(n.name)
	lfnCount := lfnEntryCount(n.name)
	checksum := lfnChecksum(to83(n.name))

	for i := lfnCount; i >= 1; i-- {
		entry := make([]byte, 32)
		entry[0] = byte(i)
		if i == lfnCount {
			entry[0] |= 0x40 // last LFN entry
		}
		entry[11] = 0x0F // LFN attribute
		entry[13] = checksum

		chunk := make([]rune, 13)
		for j := range chunk {
			chunk[j] = 0xFFFF
		}
		start := (i - 1) * 13
		for j := 0; j < 13 && start+j < len(runes); j++ {
			chunk[j] = runes[start+j]
		}
		if start+13 > len(runes) && start <= len(runes) {
			// null terminate
			if start < len(runes) {
				chunk[len(runes)-start] = 0
			}
		}

		u16 := utf16.Encode(chunk)
		for j := 0; j < 5 && j < len(u16); j++ {
			binary.LittleEndian.PutUint16(entry[1+j*2:], u16[j])
		}
		for j := 0; j < 6 && j+5 < len(u16); j++ {
			binary.LittleEndian.PutUint16(entry[14+j*2:], u16[j+5])
		}
		for j := 0; j < 2 && j+11 < len(u16); j++ {
			binary.LittleEndian.PutUint16(entry[28+j*2:], u16[j+11])
		}
		out = append(out, entry...)
	}

	// Short (8.3) entry
	e := make([]byte, 32)
	short := to83(n.name)
	copy(e[0:11], short[:])

	date, tim := fatTime(n.modTime)
	if n.isDir {
		e[11] = 0x10
	}
	binary.LittleEndian.PutUint16(e[14:], tim) // create time
	binary.LittleEndian.PutUint16(e[16:], date)
	binary.LittleEndian.PutUint16(e[18:], date) // access date
	binary.LittleEndian.PutUint16(e[22:], tim)  // write time
	binary.LittleEndian.PutUint16(e[24:], date)
	binary.LittleEndian.PutUint16(e[20:], uint16(n.startCluster>>16))
	binary.LittleEndian.PutUint16(e[26:], uint16(n.startCluster&0xFFFF))
	if !n.isDir {
		binary.LittleEndian.PutUint32(e[28:], n.size)
	}
	out = append(out, e...)
	return out
}

func lfnChecksum(name [11]byte) byte {
	var sum byte
	for _, c := range name {
		sum = (sum>>1 | sum<<7) + c
	}
	return sum
}

// ------------------------------------------------------------
// UBLK Serving
// ------------------------------------------------------------

type UblkServer struct {
	dev          *ublk.Device
	handler      *ublk.ReaderAtHandler
	DeleteEvents <-chan string
	closeChan    chan struct{}
}

// ServeVirtualFAT initializes the virtual FAT filesystem and serves it as a UBLK block device.
func ServeVirtualFAT(sourceDir string, label string) (*UblkServer, error) {
	vfat, err := NewVirtualFAT(sourceDir, label)
	if err != nil {
		return nil, fmt.Errorf("build vfat: %v", err)
	}

	handler := ublk.NewReaderAtHandler(readerAtWrapper{vfat}, ublk.ReaderAtHandlerOptions{})

	dev, err := ublk.NewDevice(ublk.DeviceOptions{
		Queues:     1,
		QueueDepth: 64,
	})
	if err != nil {
		handler.Close()
		return nil, fmt.Errorf("ublk new device: %v", err)
	}

	err = dev.SetParams(&ublk.Params{
		Types: ublk.ParamTypeBasic,
		Basic: ublk.ParamBasic{
			LogicalBSShift:  9,  // 512 B logical
			PhysicalBSShift: 9,  // Match logical sector size for legacy hosts
			IOOptShift:      12,
			IOMinShift:      9,
			MaxSectors:      128,
			DevSectors:      vfat.totalSectors,
		},
	})
	if err != nil {
		dev.Delete()
		handler.Close()
		return nil, fmt.Errorf("set params: %v", err)
	}

	go func() {
		if err := dev.Serve(handler); err != nil {
			log.Printf("ublk serve ended: %v", err)
		}
	}()

	return &UblkServer{
		dev:          dev,
		handler:      handler,
		DeleteEvents: vfat.DeleteEvents,
		closeChan:    make(chan struct{}),
	}, nil
}

// Close stops the UBLK device and cleans up resources.
func (s *UblkServer) Close() {
	if s.dev != nil {
		s.dev.Delete()
	}
	if s.handler != nil {
		s.handler.Close()
	}
	if s.closeChan != nil {
		close(s.closeChan)
	}
}

// Closed returns a channel that is closed when the UblkServer is closed.
func (s *UblkServer) Closed() <-chan struct{} {
	return s.closeChan
}

// DevPath returns the block device path (e.g. /dev/ublkb0).
func (s *UblkServer) DevPath() string {
	if s.dev != nil {
		return s.dev.BlockDevPath()
	}
	return ""
}
