package xmodem

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"sync/atomic"
	"time"
)

type modemMode int

var WrongModemType = errors.New("wrong modem type")
var TooLongFileInfo = errors.New("too long file info")
var NAKTenTimes = errors.New("NAK*10")
var FileTooLong = errors.New("file too long")
var GModeWithWrong = errors.New("g mode with wrong")
var IOCan = errors.New("send/receive break")

//var UnknownPack = errors.New("unknown pack")

const (
	XModem modemMode = iota
	YModem
	//ZModem // current don't support ZModem
)

type ModemFn uint32

const (
	ModemFn1k ModemFn = 1 << iota
	ModemFnCRC
	ModemFnCANCAN
	ModemFnBatch
	ModemFnG
	ModemXMin = 0
	ModemXMax = ModemXMin | ModemFn1k | ModemFnCRC | ModemFnCANCAN
	ModemYMin = ModemXMax | ModemFnBatch
	ModemYMax = ModemYMin | ModemFnG
)

type ModemConfig struct {
	mode modemMode
	fn   ModemFn
}

func XModemConfig(fn ModemFn) ModemConfig {
	return ModemConfig{
		mode: XModem,
		fn:   (fn & ModemXMax) | ModemXMin,
	}
}

func YModemConfig(fn ModemFn) ModemConfig {
	return ModemConfig{
		mode: YModem,
		fn:   (fn & ModemYMax) | ModemYMin,
	}
}

type Modem struct {
	termR      io.Writer
	termWW     io.Writer
	transportR *bufio.Reader
	transportW io.Writer
	finishChan chan bool
	state      *int64
	Config     ModemConfig
}

// NewModem create a modem adapter over a (reader, writer), return the modem and a filtered (reader, writer).
func NewModem(config ModemConfig, reader io.Reader, writer io.Writer) (*Modem, io.Reader, io.Writer) {
	rr, rw := io.Pipe()
	wr, ww := io.Pipe()

	mrr, mrw := io.Pipe()

	modemR := bufio.NewReader(mrr)

	finishChan := make(chan bool, 1)
	modemState := new(int64)
	*modemState = 0

	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := reader.Read(buf)
			if err != nil && err != io.EOF {
				rw.CloseWithError(err)
				mrw.CloseWithError(err)
				return
			}
			if atomic.LoadInt64(modemState) == 0 {
				rw.Write(buf[:n])
				go func() {
					modemR.Read(make([]byte, n))
				}()
				mrw.Write(buf[:n])
			} else {
				mrw.Write(buf[:n])
			}
			if err == io.EOF {
				rw.Close()
				mrw.Close()
				return
			}
		}
	}()

	go func() {
		buf := make([]byte, 1024)
		cache := &bytes.Buffer{}
		for {
			select {
			case <-finishChan:
				writer.Write(cache.Bytes())
				cache.Reset()
				atomic.StoreInt64(modemState, 0)
				go func() {
					modemR.Read(make([]byte, 1024))
				}()
				break
			default:
				n, err := wr.Read(buf)
				if err != nil && err != io.EOF {
					return
				}
				if atomic.LoadInt64(modemState) == 0 {
					writer.Write(buf[:n])
				} else {
					cache.Write(buf[:n])
				}
				if err == io.EOF {
					return
				}
			}
		}
	}()

	modem := &Modem{
		termR:      rw,
		termWW:     ww,
		transportR: modemR,
		transportW: writer,
		finishChan: finishChan,
		state:      modemState,
		Config:     config,
	}

	return modem, rr, ww
}

const (
	charSOH byte = 0x01
	charSTX byte = 0x02
	charEOT byte = 0x04
	charACK byte = 0x06
	charNAK byte = 0x15
	charCAN byte = 0x18
	charSUB byte = 0x1A
	charCRC byte = 'C'
	charG   byte = 'G'
)

func checksum(data []byte) []byte {
	sum := byte(0)
	for _, i := range data {
		sum += i
	}
	return []byte{sum}
}

func crc16(data []byte) []byte {
	crc := uint16(0)
	for _, i := range data {
		crc = crc ^ (uint16(i) << 8)
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc = crc << 1
			}
		}
	}
	return []byte{byte(crc >> 8), byte(crc & 0xff)}
}

func (m *Modem) waitWorkMode() (byte, error) {
	workMode := charNAK
	for {
		rBuf := make([]byte, 1)
		_, err := m.transportR.Read(rBuf)
		if err != nil {
			return 0, err
		}
		if rBuf[0] == charNAK || (m.Config.fn&ModemFnCRC != 0 && rBuf[0] == charCRC) || (m.Config.fn&ModemFnG != 0 && rBuf[0] == charG) {
			workMode = rBuf[0]
			break
		}
		m.termR.Write(rBuf[:1])
	}
	return workMode, nil
}

func (m *Modem) sendPack(index uint8, data []byte, mode byte) error {
	header := charSOH
	if len(data) == 1024 {
		header = charSTX
	}
	buf := append([]byte{header, index, index ^ 0xff}, data...)
	if mode == charCRC || mode == charG {
		buf = append(buf, crc16(data)...)
	} else {
		buf = append(buf, checksum(data)...)
	}
	rBuf := make([]byte, 1)
	count := 0
	can := 0
	for {
		m.transportW.Write(buf)
		if mode == charG {
			break
		}
		_, err := m.transportR.Read(rBuf)
		if err != nil {
			return err
		}
		if rBuf[0] == charCAN {
			can += 1
			if can >= 2 {
				return IOCan
			}
		} else {
			can = 0
			if rBuf[0] == charACK {
				break
			} else if rBuf[0] == charNAK {
				count += 1
				if count >= 10 {
					return NAKTenTimes
				}
			} else {
				m.termR.Write(rBuf[:1])
			}
		}
	}
	return nil
}

func (m *Modem) sendEOT() error {
	rBuf := make([]byte, 1)
	count := 0
	can := 0
	for {
		m.transportW.Write([]byte{charEOT})
		_, err := m.transportR.Read(rBuf)
		if err != nil {
			return err
		}
		if rBuf[0] == charCAN {
			can += 1
			if can >= 2 {
				return IOCan
			}
		} else {
			can = 0
			if rBuf[0] == charACK {
				break
			} else if rBuf[0] == charNAK {
				count += 1
				if count >= 10 {
					return NAKTenTimes
				}
			} else {
				m.termR.Write(rBuf[:1])
			}
		}
	}
	return nil
}

func (m *Modem) SendBreak() error {
	if m.Config.fn&ModemFnCANCAN != 0 {
		m.transportW.Write([]byte{charCAN, charCAN})
	} else {
		return m.sendEOT()
	}
	return nil
}

// SendBytes send a file.
func (m *Modem) SendBytes(file io.Reader) error {
	atomic.StoreInt64(m.state, 1)
	m.transportR.UnreadByte()
	err := m.sendBytes(file)
	if err != nil && err != io.EOF && err != TooLongFileInfo && err != IOCan {
		m.SendBreak()
	}
	m.finishChan <- true
	// force flush cache
	m.termWW.Write([]byte{})
	return err
}

func (m *Modem) sendBytes(file io.Reader) error {
	workMode, err := m.waitWorkMode()
	if err != nil {
		return err
	}
	return m.sendBuffer(file, 0, workMode)
}

type File struct {
	Path    string
	Length  int64
	ModTime time.Time
	Mode    fs.FileMode
	Body    io.Reader
}

// SendList send a list of files, only for YModem.
func (m *Modem) SendList(files []File) error {
	atomic.StoreInt64(m.state, 1)
	m.transportR.UnreadByte()
	err := m.sendList(files)
	if err != nil && err != io.EOF && err != TooLongFileInfo {
		m.SendBreak()
	}
	m.finishChan <- true
	// force flush cache
	m.termWW.Write([]byte{})
	return err
}

func (m *Modem) sendList(files []File) error {
	if m.Config.mode == XModem {
		return WrongModemType
	}
	if m.Config.mode == YModem && (m.Config.fn&ModemFnBatch) == 0 {
		return WrongModemType
	}
	for _, file := range files {
		workMode, err := m.waitWorkMode()
		if err != nil {
			return err
		}
		// make info string
		info := append([]byte(file.Path), 0)
		info = append(info, fmt.Sprintf("%d %o %o", file.Length, file.ModTime.Unix(), file.Mode&fs.ModePerm)...)
		info = append(info, 0)
		// check length legal
		tooLong := false
		if len(info) > 128 && (m.Config.fn&ModemFn1k) == 0 {
			tooLong = true
		}
		if len(info) > 1024 {
			tooLong = true
		}
		if tooLong {
			info = []byte{}
		}
		// pad NUL
		if len(info) <= 128 {
			info = append(info, make([]byte, 0, 128-len(info))...)
		} else {
			info = append(info, make([]byte, 0, 1024-len(info))...)
		}
		// send file info
		err = m.sendPack(0, info, workMode)
		if err != nil {
			return err
		}
		if tooLong {
			return TooLongFileInfo
		}
		// send body
		err = m.sendBuffer(file.Body, file.Length, workMode)
		if err != nil {
			return err
		}
	}
	workMode, err := m.waitWorkMode()
	if err != nil {
		return err
	}
	return m.sendPack(0, make([]byte, 0, 128), workMode)
}

func (m *Modem) sendBuffer(file io.Reader, maxsize int64, workMode byte) error {
	buf := make([]byte, 128)
	if m.Config.fn&ModemFn1k != 0 {
		buf = make([]byte, 1024)
	}
	total := 0
	index := 1
	for {
		n, err := io.ReadAtLeast(file, buf, len(buf))
		if err == io.EOF && n == 0 {
			return m.sendEOT()
		}
		if err != nil && err != io.ErrUnexpectedEOF {
			return err
		}
		total += n
		if maxsize > 0 && int64(total) > maxsize {
			return FileTooLong
		}
		fin := false
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			fin = true
			if n <= 128 {
				buf = buf[:128]
			}
			for i := n; i < len(buf); i++ {
				buf[i] = charSUB
			}
		}
		err = m.sendPack(byte(index&0xff), buf, workMode)
		index += 1
		if err != nil {
			return err
		}
		if fin {
			return m.sendEOT()
		}
	}
}

func (m *Modem) tryWorkMode() (byte, error) {
	var err error
	if m.Config.fn&ModemFnG != 0 {
		for i := 0; i < 3; i++ {
			m.transportW.Write([]byte{charG})
			_, err = m.transportR.Peek(1)
			if err == nil {
				return charG, nil
			} else if err != io.EOF {
				continue
			} else {
				return 0, err
			}
		}
	}
	if m.Config.fn&ModemFnCRC != 0 {
		for i := 0; i < 3; i++ {
			m.transportW.Write([]byte{charCRC})
			_, err = m.transportR.Peek(1)
			if err == nil {
				return charCRC, nil
			} else if err != io.EOF {
				continue
			} else {
				return 0, err
			}
		}
	}
	for i := 0; i < 3; i++ {
		m.transportW.Write([]byte{charNAK})
		_, err = m.transportR.Peek(1)
		if err == nil {
			return charNAK, nil
		} else if err != io.EOF {
			continue
		} else {
			return 0, err
		}
	}
	return 0, err
}

func (m *Modem) receivePack(index byte, workMode byte) ([]byte, error) {
	n := 2
	if workMode == charNAK {
		n += 1
	} else {
		n += 2
	}
	for {
		rBuf := make([]byte, 1)
		_, err := m.transportR.Read(rBuf)
		if err != nil {
			return nil, err
		}
		if rBuf[0] == charSOH || rBuf[0] == charSTX {
			bn := 128
			if rBuf[0] == charSTX {
				bn += 1024
			}
			buf := make([]byte, n+bn)
			_, err := m.transportR.Read(buf)
			if err != nil {
				return nil, err
			}
			if buf[0]^buf[1] != 0xff || buf[0] != index {
				if workMode != charG {
					m.transportW.Write([]byte{charNAK})
				} else {
					return nil, GModeWithWrong
				}
			}
			if workMode == charNAK {
				if checksum(buf[2 : 2+bn])[0] != buf[2+bn] {
					m.transportW.Write([]byte{charNAK})
				}
			} else {
				crc := crc16(buf[2 : 2+bn])
				if crc[0] != buf[2+bn] || crc[1] != buf[3+bn] {
					if workMode != charG {
						m.transportW.Write([]byte{charNAK})
					} else {
						return nil, GModeWithWrong
					}
				}
			}
			if workMode != charG {
				m.transportW.Write([]byte{charACK})
			}
			return buf[2 : 2+bn], nil
		} else if rBuf[0] == charEOT {
			return []byte{}, io.EOF
		} else {
			m.termR.Write(rBuf[:1])
		}
	}
}

func parseFileInfo(buf []byte) (*File, error) {
	ret := &File{
		Length:  0,
		ModTime: time.Unix(0, 0),
		Mode:    fs.ModePerm,
	}
	bbuf := bytes.NewBuffer(buf)
	line, err := bbuf.ReadBytes(0)
	if err != nil {
		return nil, err
	}
	ret.Path = string(line)
	modTime := int64(0)
	mode := uint32(0)
	_, err = fmt.Sscanf(string(buf[len(line)+1:]), "%d%o%o", &ret.Length, &modTime, &mode)
	if err == nil {
		ret.ModTime = time.Unix(modTime, 0)
		ret.Mode = fs.FileMode(mode) & fs.ModePerm
	}
	return ret, nil
}

type Receiver func(file File)

// Receive receive file/files for any config.
func (m *Modem) Receive(fn Receiver) error {
	atomic.StoreInt64(m.state, 1)
	err := m.receive(fn)
	m.finishChan <- true
	// force flush cache
	m.termWW.Write([]byte{})
	return err
}

func (m *Modem) receive(fn Receiver) error {
	//ret := []File{}
	for {
		workMode, err := m.tryWorkMode()
		if err != nil {
			return err
		}
		file := &File{}
		if m.Config.fn&ModemFnBatch != 0 {
			data, err := m.receivePack(0, workMode)
			if err != nil {
				return err
			}
			file, err = parseFileInfo(data)
			if err != nil {
				return err
			}
		}
		index := byte(1)
		//body := &bytes.Buffer{}
		br, bw := io.Pipe()
		file.Body = br
		go func() {
			fn(*file)
		}()
		writed := int64(0)
		for {
			data, err := m.receivePack(index, workMode)
			if err != nil && err != io.EOF {
				bw.Close()
				return err
			}
			index += 1
			if file.Length > 0 && int64(len(data)) > file.Length-writed {
				data = data[:file.Length-writed]
			}
			writed += int64(len(data))
			bw.Write(data)
			if err == io.EOF {
				m.transportW.Write([]byte{charACK})
				break
			}
		}
		bw.Close()
		if m.Config.fn&ModemFnBatch == 0 {
			break
		}
	}
	return nil
}
