package pcap

import (
	"container/list"
	"fmt"
	"github.com/op/go-logging"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	. "github.com/google/gopacket/layers"

	"gitlab.x.lan/yunshan/droplet-libs/datatype"
	"gitlab.x.lan/yunshan/droplet-libs/queue"
	. "gitlab.x.lan/yunshan/droplet-libs/utils"
	"gitlab.x.lan/yunshan/droplet-libs/zerodoc"
)

const (
	QUEUE_BATCH_SIZE = 1024
	BROADCAST_MAC    = datatype.MacInt(^uint64(0) >> 16)
	BROADCAST_IP     = datatype.IPv4Int(^uint32(0))
)

type WriterKey uint64

func getWriterIpv6Key(ip net.IP, aclGID datatype.ACLID, tapType zerodoc.TAPTypeEnum) WriterKey {
	ipHash := uint32(0)
	for i := 0; i < len(ip); i += 4 {
		ipHash ^= *(*uint32)(unsafe.Pointer(&ip[i]))
	}
	return WriterKey((uint64(ipHash) << 32) | (uint64(aclGID) << 16) | uint64(tapType))
}

func getWriterKey(ipInt datatype.IPv4Int, aclGID datatype.ACLID, tapType zerodoc.TAPTypeEnum) WriterKey {
	return WriterKey((uint64(ipInt) << 32) | (uint64(aclGID) << 16) | uint64(tapType))
}

type WrappedWriter struct {
	*Writer

	tapType zerodoc.TAPTypeEnum
	aclGID  datatype.ACLID
	ip      datatype.IPv4Int
	ip6     net.IP
	mac     datatype.MacInt
	tid     int

	tempFilename    string
	firstPacketTime time.Duration
	lastPacketTime  time.Duration
}

type WorkerCounter struct {
	FileCreations        uint64 `statsd:"file_creations"`
	FileCloses           uint64 `statsd:"file_closes"`
	FileRejections       uint64 `statsd:"file_rejections"`
	FileCreationFailures uint64 `statsd:"file_creation_failures"`
	FileWritingFailures  uint64 `statsd:"file_writing_failures"`
	BufferedCount        uint64 `statsd:"buffered_count"`
	WrittenCount         uint64 `statsd:"written_count"`
	BufferedBytes        uint64 `statsd:"buffered_bytes"`
	WrittenBytes         uint64 `statsd:"written_bytes"`
}

type Worker struct {
	inputQueue queue.MultiQueueReader
	index      int
	queueKey   queue.HashKey

	maxConcurrentFiles int
	maxFileSize        int64
	maxFilePeriod      time.Duration
	baseDirectory      string

	*WorkerCounter

	writers     map[WriterKey]*WrappedWriter
	writersIpv6 map[WriterKey]*list.List

	writerBufferSize int
	tcpipChecksum    bool

	exiting bool
	exited  bool
	exitWg  *sync.WaitGroup
}

func (m *WorkerManager) newWorker(index int) *Worker {
	return &Worker{
		inputQueue: m.inputQueue,
		index:      index,
		queueKey:   queue.HashKey(uint8(index)),

		maxConcurrentFiles: m.maxConcurrentFiles / m.nQueues,
		maxFileSize:        int64(m.maxFileSizeMB) << 20,
		maxFilePeriod:      time.Duration(m.maxFilePeriodSecond) * time.Second,
		baseDirectory:      m.baseDirectory,

		WorkerCounter: &WorkerCounter{},

		writers:     make(map[WriterKey]*WrappedWriter),
		writersIpv6: make(map[WriterKey]*list.List),

		writerBufferSize: m.blockSizeKB << 10,
		tcpipChecksum:    m.tcpipChecksum,

		exiting: false,
		exited:  false,
		exitWg:  &sync.WaitGroup{},
	}
}

func isISP(inPort uint32) bool {
	return 0x10000 <= inPort && inPort < 0x20000
}

func isTOR(inPort uint32) bool {
	return 0x30000 <= inPort && inPort < 0x40000
}

func macToString(mac datatype.MacInt) string {
	return fmt.Sprintf("%012x", mac)
}

func ipToString(ip datatype.IPv4Int) string {
	return fmt.Sprintf("%03d%03d%03d%03d", uint8(ip>>24), uint8(ip>>16), uint8(ip>>8), uint8(ip))
}

func tapTypeToString(tapType zerodoc.TAPTypeEnum) string {
	if tapType == 3 {
		return "tor"
	}
	if tapType >= 0 && tapType <= 30 {
		return fmt.Sprintf("isp%d", tapType)
	}
	panic(fmt.Sprintf("unsupported tap type %d", tapType))
}

func formatDuration(d time.Duration) string {
	return time.Unix(0, int64(d)).Format(TIME_FORMAT)
}

func getTempFilename(tapType zerodoc.TAPTypeEnum, mac datatype.MacInt, ip datatype.IPv4Int, firstPacketTime time.Duration, index int) string {
	return fmt.Sprintf("%s_%s_%s_%s_.%d.pcap.temp", tapTypeToString(tapType), macToString(mac), ipToString(ip), formatDuration(firstPacketTime), index)
}

func getTempFilenameByIpv6(tapType zerodoc.TAPTypeEnum, mac datatype.MacInt, ip net.IP, firstPacketTime time.Duration, index int) string {
	return fmt.Sprintf("%s_%s_%s_%s_.%d.pcap.temp", tapTypeToString(tapType), macToString(mac), ip, formatDuration(firstPacketTime), index)
}

func (w *WrappedWriter) getTempFilename(base string) string {
	if w.ip6 == nil {
		return fmt.Sprintf("%s/%d/%s", base, w.aclGID, getTempFilename(w.tapType, w.mac, w.ip, w.firstPacketTime, w.tid))
	} else {
		return fmt.Sprintf("%s/%d/%s", base, w.aclGID, getTempFilenameByIpv6(w.tapType, w.mac, w.ip6, w.firstPacketTime, w.tid))
	}
}

func (w *WrappedWriter) getFilename(base string) string {
	ipString := ""
	if w.ip6 == nil {
		ipString = ipToString(w.ip)
	} else {
		ipString = w.ip6.String()
	}
	return fmt.Sprintf("%s/%d/%s_%s_%s_%s_%s.%d.pcap", base, w.aclGID, tapTypeToString(w.tapType), macToString(w.mac), ipString, formatDuration(w.firstPacketTime), formatDuration(w.lastPacketTime), w.tid)
}

func (w *Worker) shouldCloseFile(writer *WrappedWriter, packet *datatype.MetaPacket) bool {
	// check for file size and time
	if writer.FileSize()+int64(writer.BufferSize()) >= w.maxFileSize {
		return true
	}
	if packet.Timestamp-writer.firstPacketTime > w.maxFilePeriod {
		return true
	}
	return false
}

func (w *Worker) finishWriter(writer *WrappedWriter, newFilename string) {
	writer.Close()
	counter := writer.GetAndResetStats()
	w.BufferedCount += counter.totalBufferedCount
	w.WrittenCount += counter.totalWrittenCount
	w.BufferedBytes += counter.totalBufferedBytes
	w.WrittenBytes += counter.totalWrittenBytes
	log.Debugf("Finish writing %s, renaming to %s", writer.tempFilename, newFilename)
	os.Rename(writer.tempFilename, newFilename)
	w.FileCloses++
}

func (w *Worker) writePacket(packet *datatype.MetaPacket, tapType zerodoc.TAPTypeEnum, ip datatype.IPv4Int, mac datatype.MacInt, aclGID datatype.ACLID) {
	key := getWriterKey(ip, aclGID, tapType)
	writer, exist := w.writers[key]
	if exist && w.shouldCloseFile(writer, packet) {
		newFilename := writer.getFilename(w.baseDirectory)
		w.finishWriter(writer, newFilename)
		delete(w.writers, key)
		exist = false
	}
	if !exist {
		writer = w.generateWrappedWriter(IpFromUint32(ip), mac, tapType, aclGID, packet.Timestamp)
		if writer == nil {
			return
		}
		w.writers[key] = writer
	}
	if err := writer.Write(packet); err != nil {
		log.Debugf("Failed to write packet to %s: %s", writer.tempFilename, err)
		w.FileWritingFailures++
		return
	}
	counter := writer.GetAndResetStats()
	w.BufferedCount += counter.totalBufferedCount
	w.WrittenCount += counter.totalWrittenCount
	w.BufferedBytes += counter.totalBufferedBytes
	w.WrittenBytes += counter.totalWrittenBytes
	writer.lastPacketTime = packet.Timestamp
}

func (w *Worker) generateWrappedWriter(ip net.IP, mac datatype.MacInt, tapType zerodoc.TAPTypeEnum, aclGID datatype.ACLID, timestamp time.Duration) *WrappedWriter {
	if len(w.writers) >= w.maxConcurrentFiles {
		if log.IsEnabledFor(logging.DEBUG) {
			log.Debugf("Max concurrent file (%d files) exceeded", w.maxConcurrentFiles)
		}
		w.FileRejections++
		return nil
	}

	directory := fmt.Sprintf("%s/%d", w.baseDirectory, aclGID)
	if _, err := os.Stat(directory); os.IsNotExist(err) {
		os.MkdirAll(directory, os.ModePerm)
	}
	writer := &WrappedWriter{
		tapType:         tapType,
		aclGID:          aclGID,
		mac:             mac,
		tid:             w.index,
		firstPacketTime: timestamp,
		lastPacketTime:  timestamp,
	}
	if ip.To4() != nil {
		writer.ip = IpToUint32(ip)
	} else {
		writer.ip6 = ip
	}

	writer.tempFilename = writer.getTempFilename(w.baseDirectory)
	if log.IsEnabledFor(logging.DEBUG) {
		log.Debugf("Begin to write packets to %s", writer.tempFilename)
	}
	var err error
	if writer.Writer, err = NewWriter(writer.tempFilename, w.writerBufferSize, w.tcpipChecksum); err != nil {
		if log.IsEnabledFor(logging.DEBUG) {
			log.Debugf("Failed to create writer for %s: %s", writer.tempFilename, err)
		}
		w.FileCreationFailures++
		return nil
	}
	w.FileCreations++
	return writer
}

func (w *Worker) getWrappedWriter(ip net.IP, mac datatype.MacInt, tapType zerodoc.TAPTypeEnum, aclGID datatype.ACLID, packet *datatype.MetaPacket) *WrappedWriter {
	var element *list.Element
	var result *WrappedWriter

	key := getWriterIpv6Key(ip, aclGID, tapType)
	writerList, exist := w.writersIpv6[key]
	if exist {
		for e := writerList.Front(); e != nil; e = e.Next() {
			writer := e.Value.(*WrappedWriter)
			if writer.ip6.Equal(ip) {
				element = e
				result = writer
				break
			}
		}
	} else {
		writerList = list.New()
		w.writersIpv6[key] = writerList
	}

	if result != nil && w.shouldCloseFile(result, packet) {
		newFilename := result.getFilename(w.baseDirectory)
		w.finishWriter(result, newFilename)
		writerList.Remove(element)
		result = nil
	}

	if result == nil {
		result = w.generateWrappedWriter(ip, mac, tapType, aclGID, packet.Timestamp)
		if result != nil {
			writerList.PushBack(result)
		}
	}
	return result
}

func (w *Worker) writePacketIpv6(packet *datatype.MetaPacket, tapType zerodoc.TAPTypeEnum, ip net.IP, mac datatype.MacInt, aclGID datatype.ACLID) {
	writer := w.getWrappedWriter(ip, mac, tapType, aclGID, packet)
	if writer == nil {
		return
	}

	if err := writer.Write(packet); err != nil {
		log.Debugf("Failed to write packet to %s: %s", writer.tempFilename, err)
		w.FileWritingFailures++
		return
	}
	counter := writer.GetAndResetStats()
	w.BufferedCount += counter.totalBufferedCount
	w.WrittenCount += counter.totalWrittenCount
	w.BufferedBytes += counter.totalBufferedBytes
	w.WrittenBytes += counter.totalWrittenBytes
	writer.lastPacketTime = packet.Timestamp
}

func (w *Worker) Process() {
	elements := make([]interface{}, QUEUE_BATCH_SIZE)
	ips := make([]datatype.IPv4Int, 0, 2)
	macs := make([]datatype.MacInt, 0, 2)
	ip6s := make([]net.IP, 0, 2)

WORKING_LOOP:
	for !w.exiting {
		n := w.inputQueue.Gets(w.queueKey, elements)
		timeNow := time.Duration(time.Now().UnixNano())
		for _, e := range elements[:n] {
			if e == nil { // tick
				if w.exiting {
					break WORKING_LOOP
				}
				for key, writer := range w.writers {
					if timeNow-writer.firstPacketTime > w.maxFilePeriod {
						newFilename := writer.getFilename(w.baseDirectory)
						w.finishWriter(writer, newFilename)
						delete(w.writers, key)
					}
				}

				for _, writerList := range w.writersIpv6 {
					for e := writerList.Front(); e != nil; {
						r, writer := e, e.Value.(*WrappedWriter)
						e = e.Next()
						if timeNow-writer.firstPacketTime > w.maxFilePeriod {
							newFilename := writer.getFilename(w.baseDirectory)
							w.finishWriter(writer, newFilename)
							writerList.Remove(r)
						}
					}
				}
				continue
			}

			packet := e.(*datatype.MetaPacket)

			if packet.PolicyData == nil || packet.EndpointData == nil { // shouldn't happen
				log.Warningf("drop invalid packet with nil PolicyData or EndpointData %v", packet)
				datatype.ReleaseMetaPacket(packet)
				continue
			}

			ips = ips[:0]
			macs = macs[:0]
			ip6s = ip6s[:0]
			var tapType zerodoc.TAPTypeEnum
			if isISP(packet.InPort) {
				tapType = zerodoc.TAPTypeEnum(packet.InPort - 0x10000)
				if packet.EthType != EthernetTypeIPv6 {
					if packet.EndpointData.SrcInfo.L3EpcId != 0 && packet.IpSrc != BROADCAST_IP && packet.MacSrc != BROADCAST_MAC {
						ips = append(ips, packet.IpSrc)
						macs = append(macs, packet.MacSrc)
					}
					if packet.EndpointData.DstInfo.L3EpcId != 0 && packet.IpDst != BROADCAST_IP && packet.MacDst != BROADCAST_MAC {
						ips = append(ips, packet.IpDst)
						macs = append(macs, packet.MacDst)
					}
				} else {
					if packet.EndpointData.SrcInfo.L3EpcId != 0 && packet.Ip6Src.IsMulticast() && packet.MacSrc != BROADCAST_MAC {
						ip6s = append(ip6s, packet.Ip6Src)
						macs = append(macs, packet.MacSrc)
					}
					if packet.EndpointData.DstInfo.L3EpcId != 0 && packet.Ip6Dst.IsMulticast() && packet.MacDst != BROADCAST_MAC {
						ip6s = append(ip6s, packet.Ip6Dst)
						macs = append(macs, packet.MacDst)
					}
				}
			} else if isTOR(packet.InPort) {
				tapType = zerodoc.ToR
				if packet.EthType != EthernetTypeIPv6 {
					if (packet.L2End0 || packet.EndpointData.SrcInfo.L2End) && packet.IpSrc != BROADCAST_IP && packet.MacSrc != BROADCAST_MAC {
						ips = append(ips, packet.IpSrc)
						macs = append(macs, packet.MacSrc)
					}
					if (packet.L2End1 || packet.EndpointData.DstInfo.L2End) && packet.IpDst != BROADCAST_IP && packet.MacDst != BROADCAST_MAC {
						ips = append(ips, packet.IpDst)
						macs = append(macs, packet.MacDst)
					}
				} else {
					if (packet.L2End0 || packet.EndpointData.SrcInfo.L2End) && !packet.Ip6Src.IsMulticast() && packet.MacSrc != BROADCAST_MAC {
						ip6s = append(ip6s, packet.Ip6Src)
						macs = append(macs, packet.MacSrc)
					}
					if (packet.L2End1 || packet.EndpointData.DstInfo.L2End) && !packet.Ip6Dst.IsMulticast() && packet.MacDst != BROADCAST_MAC {
						ip6s = append(ip6s, packet.Ip6Dst)
						macs = append(macs, packet.MacDst)
					}
				}
			} else {
				datatype.ReleaseMetaPacket(packet)
				continue
			}

			for _, policy := range packet.PolicyData.AclActions {
				if policy.GetACLGID() <= 0 {
					continue
				}
				if policy.GetActionFlags()&datatype.ACTION_PACKET_CAPTURING != 0 {
					if packet.EthType != EthernetTypeIPv6 {
						for i := range ips {
							w.writePacket(packet, tapType, ips[i], macs[i], policy.GetACLGID())
						}
					} else {
						for i := range ip6s {
							w.writePacketIpv6(packet, tapType, ip6s[i], macs[i], policy.GetACLGID())
						}
					}
				}
			}

			datatype.ReleaseMetaPacket(packet)
		}
	}

	for _, writer := range w.writers {
		newFilename := writer.getFilename(w.baseDirectory)
		w.finishWriter(writer, newFilename)
	}
	for _, writerList := range w.writersIpv6 {
		for e := writerList.Front(); e != nil; e = e.Next() {
			writer := e.Value.(*WrappedWriter)
			newFilename := writer.getFilename(w.baseDirectory)
			w.finishWriter(writer, newFilename)
		}
	}
	log.Infof("Stopped pcap worker (%d)", w.index)
	w.exitWg.Done()
}

func (w *Worker) Close() error {
	log.Infof("Stop pcap worker (%d) writing to %d files", w.index, len(w.writers))
	w.exitWg.Add(1)
	w.exiting = true
	w.exitWg.Wait()
	w.exited = true
	return nil
}

func (w *Worker) GetCounter() interface{} {
	counter := &WorkerCounter{}
	counter, w.WorkerCounter = w.WorkerCounter, counter
	return counter
}

func (w *Worker) Closed() bool {
	return w.exited
}
