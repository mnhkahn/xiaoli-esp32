package admin

import (
	"bytes"
	"encoding/binary"
	"errors"
	"time"
)

const oggOpusSerial = 0x7869616f

var oggCRCTable = buildOggCRCTable()

func buildOggOpus(frames [][]byte, inputSampleRate int, channels int, frameDurationMS int) ([]byte, error) {
	if len(frames) == 0 {
		return nil, errors.New("no opus frames")
	}
	if inputSampleRate <= 0 {
		inputSampleRate = 16000
	}
	if channels <= 0 {
		channels = 1
	}
	if frameDurationMS <= 0 {
		frameDurationMS = 60
	}
	var out bytes.Buffer
	seq := uint32(0)
	writeOggPage(&out, 0x02, 0, oggOpusSerial, seq, opusHeadPacket(inputSampleRate, channels))
	seq++
	writeOggPage(&out, 0x00, 0, oggOpusSerial, seq, opusTagsPacket())
	seq++
	granuleStep := uint64(frameDurationMS * 48)
	granule := uint64(0)
	for i, frame := range frames {
		if len(frame) == 0 {
			continue
		}
		granule += granuleStep
		headerType := byte(0x00)
		if i == len(frames)-1 {
			headerType = 0x04
		}
		writeOggPage(&out, headerType, granule, oggOpusSerial, seq, frame)
		seq++
	}
	if out.Len() == 0 {
		return nil, errors.New("failed to build ogg")
	}
	return out.Bytes(), nil
}

func opusHeadPacket(inputSampleRate int, channels int) []byte {
	packet := make([]byte, 19)
	copy(packet, []byte("OpusHead"))
	packet[8] = 1
	packet[9] = byte(channels)
	binary.LittleEndian.PutUint16(packet[10:12], 0)
	binary.LittleEndian.PutUint32(packet[12:16], uint32(inputSampleRate))
	binary.LittleEndian.PutUint16(packet[16:18], 0)
	packet[18] = 0
	return packet
}

func opusTagsPacket() []byte {
	vendor := []byte("xiaoli-go")
	packet := make([]byte, 8+4+len(vendor)+4)
	copy(packet, []byte("OpusTags"))
	binary.LittleEndian.PutUint32(packet[8:12], uint32(len(vendor)))
	copy(packet[12:], vendor)
	binary.LittleEndian.PutUint32(packet[12+len(vendor):], 0)
	return packet
}

func writeOggPage(out *bytes.Buffer, headerType byte, granule uint64, serial uint32, seq uint32, packet []byte) {
	laces := oggLaces(len(packet))
	page := make([]byte, 27+len(laces)+len(packet))
	copy(page[0:4], []byte("OggS"))
	page[4] = 0
	page[5] = headerType
	binary.LittleEndian.PutUint64(page[6:14], granule)
	binary.LittleEndian.PutUint32(page[14:18], serial)
	binary.LittleEndian.PutUint32(page[18:22], seq)
	page[26] = byte(len(laces))
	copy(page[27:], laces)
	copy(page[27+len(laces):], packet)
	crc := oggCRC(page)
	binary.LittleEndian.PutUint32(page[22:26], crc)
	_, _ = out.Write(page)
}

func oggLaces(length int) []byte {
	if length == 0 {
		return []byte{0}
	}
	full := length / 255
	rem := length % 255
	laces := make([]byte, 0, full+1)
	for i := 0; i < full; i++ {
		laces = append(laces, 255)
	}
	if rem == 0 {
		laces = append(laces, 0)
	} else {
		laces = append(laces, byte(rem))
	}
	return laces
}

func buildOggCRCTable() [256]uint32 {
	var table [256]uint32
	for i := 0; i < 256; i++ {
		r := uint32(i) << 24
		for j := 0; j < 8; j++ {
			if r&0x80000000 != 0 {
				r = (r << 1) ^ 0x04c11db7
			} else {
				r <<= 1
			}
		}
		table[i] = r
	}
	return table
}

func oggCRC(page []byte) uint32 {
	var crc uint32
	for _, b := range page {
		crc = (crc << 8) ^ oggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

func oggOpusDuration(body []byte) time.Duration {
	reader := bytes.NewReader(body)
	var lastGranule uint64
	for reader.Len() >= 27 {
		header := make([]byte, 27)
		if _, err := reader.Read(header); err != nil {
			break
		}
		if string(header[0:4]) != "OggS" {
			break
		}
		granule := binary.LittleEndian.Uint64(header[6:14])
		segments := int(header[26])
		if segments > reader.Len() {
			break
		}
		laces := make([]byte, segments)
		if _, err := reader.Read(laces); err != nil {
			break
		}
		payloadLen := 0
		for _, lace := range laces {
			payloadLen += int(lace)
		}
		if payloadLen > reader.Len() {
			break
		}
		if _, err := reader.Seek(int64(payloadLen), 1); err != nil {
			break
		}
		if granule != ^uint64(0) && granule > lastGranule {
			lastGranule = granule
		}
	}
	if lastGranule == 0 {
		return 0
	}
	return time.Duration(float64(time.Second) * float64(lastGranule) / 48000.0)
}
