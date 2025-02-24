package astits

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/asticode/go-astikit"
)

// Sync byte
const syncByte = '\x47'

// Errors
var (
	ErrNoMorePackets                = errors.New("astits: no more packets")
	ErrPacketMustStartWithASyncByte = errors.New("astits: packet must start with a sync byte")
)

// Demuxer represents a demuxer
// https://en.wikipedia.org/wiki/MPEG_transport_stream
// http://seidl.cs.vsb.cz/download/dvb/DVB_Poster.pdf
// http://www.etsi.org/deliver/etsi_en/300400_300499/300468/01.13.01_40/en_300468v011301o.pdf
type Demuxer struct {
	ctx        context.Context
	dataBuffer []*DemuxerData
	l          astikit.CompleteLogger

	optPacketSize    int
	optPacketsParser PacketsParser
	optPacketSkipper PacketSkipper

	packetBuffer *packetBuffer
	packetPool   *packetPool
	programMap   *programMap
	streamMap    *elementaryStreamMap
	r            io.Reader
}

// PacketsParser represents an object capable of parsing a set of packets containing a unique payload spanning over those packets
// Use the skip returned argument to indicate whether the default process should still be executed on the set of packets
type PacketsParser func(ps []*Packet) (ds []*DemuxerData, skip bool, err error)

// PacketSkipper represents an object capable of skipping a packet before parsing its payload. Its header and adaptation field is parsed and provided to the object.
// Use this option if you need to filter out unwanted packets from your pipeline. NextPacket() will return the next unskipped packet if any.
type PacketSkipper func(p *Packet) (skip bool)

// NewDemuxer creates a new transport stream based on a reader
func NewDemuxer(ctx context.Context, r io.Reader, opts ...func(*Demuxer)) (d *Demuxer) {
	// Init
	d = &Demuxer{
		ctx:        ctx,
		l:          astikit.AdaptStdLogger(nil),
		programMap: newProgramMap(),
		streamMap:  newElementaryStreamMap(),
		r:          r,
	}
	d.packetPool = newPacketPool(d.programMap)

	// Apply options
	for _, opt := range opts {
		opt(d)
	}

	return
}

// DemuxerOptLogger returns the option to set the logger
func DemuxerOptLogger(l astikit.StdLogger) func(*Demuxer) {
	return func(d *Demuxer) {
		d.l = astikit.AdaptStdLogger(l)
	}
}

// DemuxerOptPacketSize returns the option to set the packet size
func DemuxerOptPacketSize(packetSize int) func(*Demuxer) {
	return func(d *Demuxer) {
		d.optPacketSize = packetSize
	}
}

// DemuxerOptPacketsParser returns the option to set the packets parser
func DemuxerOptPacketsParser(p PacketsParser) func(*Demuxer) {
	return func(d *Demuxer) {
		d.optPacketsParser = p
	}
}

// DemuxerOptPacketSkipper returns the option to set the packet skipper
func DemuxerOptPacketSkipper(s PacketSkipper) func(*Demuxer) {
	return func(d *Demuxer) {
		d.optPacketSkipper = s
	}
}

// NextPacket retrieves the next packet
func (dmx *Demuxer) NextPacket() (p *Packet, err error) {
	// Check ctx error
	// TODO Handle ctx error another way since if the read blocks, everything blocks
	// Maybe execute everything in a goroutine and listen the ctx channel in the same for loop
	if err = dmx.ctx.Err(); err != nil {
		return
	}

	// Create packet buffer if not exists
	if dmx.packetBuffer == nil {
		if dmx.packetBuffer, err = newPacketBuffer(dmx.r, dmx.optPacketSize, dmx.optPacketSkipper); err != nil {
			err = fmt.Errorf("astits: creating packet buffer failed: %w", err)
			return
		}
	}

	// Fetch next packet from buffer
	if p, err = dmx.packetBuffer.next(); err != nil {
		if err != ErrNoMorePackets {
			err = fmt.Errorf("astits: fetching next packet from buffer failed: %w", err)
		}
		return
	}
	return
}

// NextData retrieves the next data
func (dmx *Demuxer) NextData() (d *DemuxerData, err error) {
	// Check data buffer
	if len(dmx.dataBuffer) > 0 {
		d = dmx.dataBuffer[0]
		dmx.dataBuffer = dmx.dataBuffer[1:]
		return
	}

	// Loop through packets
	var p *Packet
	var ps []*Packet
	var ds []*DemuxerData
	for {
		// Get next packet
		if p, err = dmx.NextPacket(); err != nil {
			// If the end of the stream has been reached, we dump the packet pool
			if err == ErrNoMorePackets {
				for {
					// Dump packet pool
					if ps = dmx.packetPool.dumpUnlocked(); len(ps) == 0 {
						break
					}

					// Parse data
					var errParseData error
					if ds, errParseData = parseData(ps, dmx.optPacketsParser, dmx.programMap, dmx.streamMap); errParseData != nil {
						// Log error as there may be some incomplete data here
						// We still want to try to parse all packets, in case final data is complete
						dmx.l.Error(fmt.Errorf("astits: parsing data failed: %w", errParseData))
						continue
					}

					// Update data
					if d = dmx.updateData(ds); d != nil {
						err = nil
						return
					}
				}
				return
			}
			err = fmt.Errorf("astits: fetching next packet failed: %w", err)
			return
		}

		// Add packet to the pool
		if ps = dmx.packetPool.addUnlocked(p); len(ps) == 0 {
			continue
		}

		// Parse data
		if ds, err = parseData(ps, dmx.optPacketsParser, dmx.programMap, dmx.streamMap); err != nil {
			err = fmt.Errorf("astits: building new data failed: %w", err)
			return
		}

		// Update data
		if d = dmx.updateData(ds); d != nil {
			return
		}
	}
}

func (dmx *Demuxer) updateData(ds []*DemuxerData) (d *DemuxerData) {
	// Check whether there is data to be processed
	if len(ds) > 0 {
		// Process data
		d = ds[0]
		dmx.dataBuffer = append(dmx.dataBuffer, ds[1:]...)

		// Update program map
		for _, v := range ds {
			if v.PAT != nil {
				for _, pgm := range v.PAT.Programs {
					// Program number 0 is reserved to NIT
					if pgm.ProgramNumber > 0 {
						dmx.programMap.setUnlocked(pgm.ProgramMapID, pgm.ProgramNumber)
					}
				}
			}
			if v.PMT != nil {
				for _, es := range v.PMT.ElementaryStreams {
					dmx.streamMap.setLocked(es.ElementaryPID, v.PMT.ProgramNumber)
				}
			}
		}
	}
	return
}

// Rewind rewinds the demuxer reader
func (dmx *Demuxer) Rewind() (n int64, err error) {
	dmx.dataBuffer = []*DemuxerData{}
	dmx.packetBuffer = nil
	dmx.packetPool = newPacketPool(dmx.programMap)
	if n, err = rewind(dmx.r); err != nil {
		err = fmt.Errorf("astits: rewinding reader failed: %w", err)
		return
	}
	return
}
