package multiplex

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	pool "github.com/libp2p/go-buffer-pool"

	logging "github.com/ipfs/go-log/v2"
	"github.com/multiformats/go-varint"
)

var log = logging.Logger("mplex")

var MaxMessageSize = 1 << 20
var MaxBuffers = 4

// Max time to block waiting for a slow reader to read from a stream before
// resetting it. Preferably, we'd have some form of back-pressure mechanism but
// we don't have that in this protocol.
var ReceiveTimeout = 5 * time.Second

// ErrShutdown is returned when operating on a shutdown session
var ErrShutdown = errors.New("session shut down")

// ErrTwoInitiators is returned when both sides think they're the initiator
var ErrTwoInitiators = errors.New("two initiators")

// ErrInvalidState is returned when the other side does something it shouldn't.
// In this case, we close the connection to be safe.
var ErrInvalidState = errors.New("received an unexpected message from the peer")

var errTimeout = timeout{}

var (
	ResetStreamTimeout = 2 * time.Minute

	WriteCoalesceDelay = 100 * time.Microsecond
)

type timeout struct{}

func (timeout) Error() string   { return "i/o deadline exceeded" }
func (timeout) Temporary() bool { return true }
func (timeout) Timeout() bool   { return true }

// The MemoryManager allows management of memory allocations.
type MemoryManager interface {
	// ReserveMemory reserves memory / buffer.
	ReserveMemory(size int, prio uint8) error
	// ReleaseMemory explicitly releases memory previously reserved with ReserveMemory
	ReleaseMemory(size int)
}

type nullMemoryManager struct{}

func (m *nullMemoryManager) ReserveMemory(size int, prio uint8) error { return nil }
func (m *nullMemoryManager) ReleaseMemory(size int)                   {}

// +1 for initiator
const (
	newStreamTag = 0
	messageTag   = 2
	closeTag     = 4
	resetTag     = 6
)

// Multiplex is a mplex session.
type Multiplex struct {
	con       net.Conn
	buf       *bufio.Reader
	nextID    uint64
	initiator bool

	memoryManager MemoryManager

	closed       chan struct{}
	shutdown     chan struct{}
	shutdownErr  error
	shutdownLock sync.Mutex

	writeCh  chan []byte
	nstreams chan *Stream

	channels map[streamID]*Stream
	chLock   sync.Mutex

	bufIn, bufOut  chan struct{}
	bufMax         int
	reservedMemory int
}

// NewMultiplex creates a new multiplexer session.
func NewMultiplex(con net.Conn, initiator bool, memoryManager MemoryManager) (*Multiplex, error) {
	if memoryManager == nil {
		memoryManager = &nullMemoryManager{}
	}
	mp := &Multiplex{
		con:           con,
		initiator:     initiator,
		buf:           bufio.NewReader(con),
		channels:      make(map[streamID]*Stream),
		closed:        make(chan struct{}),
		shutdown:      make(chan struct{}),
		writeCh:       make(chan []byte, 16),
		nstreams:      make(chan *Stream, 16),
		memoryManager: memoryManager,
	}

	// up-front reserve memory for max buffers
	bufs := 0
	var err error
	for i := 0; i < MaxBuffers; i++ {
		var prio uint8
		switch bufs {
		case 0:
			prio = 255
		case 1:
			prio = 192
		default:
			prio = 128
		}
		if err = mp.memoryManager.ReserveMemory(2*MaxMessageSize, prio); err != nil {
			break
		}
		mp.reservedMemory += 2 * MaxMessageSize
		bufs++
	}

	if bufs == 0 {
		return nil, err
	}

	mp.bufMax = bufs
	mp.bufIn = make(chan struct{}, bufs)
	mp.bufOut = make(chan struct{}, bufs)

	go mp.handleIncoming()
	go mp.handleOutgoing()

	return mp, nil
}

func (mp *Multiplex) newStream(id streamID, name string) (s *Stream) {
	s = &Stream{
		id:          id,
		name:        name,
		dataIn:      make(chan []byte, mp.bufMax),
		rDeadline:   makePipeDeadline(),
		wDeadline:   makePipeDeadline(),
		mp:          mp,
		writeCancel: make(chan struct{}),
		readCancel:  make(chan struct{}),
	}
	return
}

// Accept accepts the next stream from the connection.
func (m *Multiplex) Accept() (*Stream, error) {
	select {
	case s, ok := <-m.nstreams:
		if !ok {
			return nil, errors.New("multiplex closed")
		}
		return s, nil
	case <-m.closed:
		return nil, m.shutdownErr
	}
}

// Close closes the session.
func (mp *Multiplex) Close() error {
	mp.closeNoWait()

	// Wait for the receive loop to finish.
	<-mp.closed

	return nil
}

func (mp *Multiplex) closeNoWait() {
	mp.shutdownLock.Lock()
	select {
	case <-mp.shutdown:
	default:
		mp.memoryManager.ReleaseMemory(mp.reservedMemory)
		mp.con.Close()
		close(mp.shutdown)
	}
	mp.shutdownLock.Unlock()
}

// IsClosed returns true if the session is closed.
func (mp *Multiplex) IsClosed() bool {
	select {
	case <-mp.closed:
		return true
	default:
		return false
	}
}

// CloseChan returns a read-only channel which will be closed when the session is closed
func (mp *Multiplex) CloseChan() <-chan struct{} {
	return mp.closed
}

func (mp *Multiplex) sendMsg(timeout, cancel <-chan struct{}, header uint64, data []byte) error {
	buf, err := mp.getBufferOutbound(len(data)+20, timeout, cancel)
	if err != nil {
		return err
	}

	n := 0
	n += binary.PutUvarint(buf[n:], header)
	n += binary.PutUvarint(buf[n:], uint64(len(data)))
	n += copy(buf[n:], data)

	select {
	case mp.writeCh <- buf[:n]:
		return nil
	case <-mp.shutdown:
		mp.putBufferOutbound(buf)
		return ErrShutdown
	case <-timeout:
		mp.putBufferOutbound(buf)
		return errTimeout
	case <-cancel:
		mp.putBufferOutbound(buf)
		return ErrStreamClosed
	}
}

func (mp *Multiplex) handleOutgoing() {
	for {
		select {
		case <-mp.shutdown:
			return

		case data := <-mp.writeCh:
			err := mp.doWriteMsg(data)
			mp.putBufferOutbound(data)
			if err != nil {
				// the connection is closed by this time
				log.Warnf("error writing data: %s", err.Error())
				return
			}
		}
	}
}

func (mp *Multiplex) doWriteMsg(data []byte) error {
	if mp.isShutdown() {
		return ErrShutdown
	}

	_, err := mp.con.Write(data)
	if err != nil {
		mp.closeNoWait()
	}

	return err
}

func (mp *Multiplex) nextChanID() uint64 {
	out := mp.nextID
	mp.nextID++
	return out
}

// NewStream creates a new stream.
func (mp *Multiplex) NewStream(ctx context.Context) (*Stream, error) {
	return mp.NewNamedStream(ctx, "")
}

// NewNamedStream creates a new named stream.
func (mp *Multiplex) NewNamedStream(ctx context.Context, name string) (*Stream, error) {
	mp.chLock.Lock()

	// We could call IsClosed but this is faster (given that we already have
	// the lock).
	if mp.channels == nil {
		mp.chLock.Unlock()
		return nil, ErrShutdown
	}

	sid := mp.nextChanID()
	header := (sid << 3) | newStreamTag

	if name == "" {
		name = fmt.Sprint(sid)
	}
	s := mp.newStream(streamID{
		id:        sid,
		initiator: true,
	}, name)
	mp.channels[s.id] = s
	mp.chLock.Unlock()

	err := mp.sendMsg(ctx.Done(), nil, header, []byte(name))
	if err != nil {
		if err == errTimeout {
			return nil, ctx.Err()
		}
		return nil, err
	}

	return s, nil
}

func (mp *Multiplex) cleanup() {
	mp.closeNoWait()

	// Take the channels.
	mp.chLock.Lock()
	channels := mp.channels
	mp.channels = nil
	mp.chLock.Unlock()

	// Cancel any reads/writes
	for _, msch := range channels {
		msch.cancelRead(ErrStreamReset)
		msch.cancelWrite(ErrStreamReset)
	}

	// And... shutdown!
	if mp.shutdownErr == nil {
		mp.shutdownErr = ErrShutdown
	}
	close(mp.closed)
}

func (mp *Multiplex) handleIncoming() {
	defer mp.cleanup()

	recvTimeout := time.NewTimer(0)
	defer recvTimeout.Stop()

	if !recvTimeout.Stop() {
		<-recvTimeout.C
	}

	for {
		chID, tag, err := mp.readNextHeader()
		if err != nil {
			mp.shutdownErr = err
			return
		}

		remoteIsInitiator := tag&1 == 0
		ch := streamID{
			// true if *I'm* the initiator.
			initiator: !remoteIsInitiator,
			id:        chID,
		}
		// Rounds up the tag:
		// 0 -> 0
		// 1 -> 2
		// 2 -> 2
		// 3 -> 4
		// etc...
		tag += (tag & 1)

		b, err := mp.readNext()
		if err != nil {
			mp.shutdownErr = err
			return
		}

		mp.chLock.Lock()
		msch, ok := mp.channels[ch]
		mp.chLock.Unlock()

		switch tag {
		case newStreamTag:
			if ok {
				log.Debugf("received NewStream message for existing stream: %d", ch)
				mp.shutdownErr = ErrInvalidState
				return
			}

			name := string(b)
			mp.putBufferInbound(b)

			msch = mp.newStream(ch, name)
			mp.chLock.Lock()
			mp.channels[ch] = msch
			mp.chLock.Unlock()
			select {
			case mp.nstreams <- msch:
			case <-mp.shutdown:
				return
			}

		case resetTag:
			if !ok {
				// This is *ok*. We forget the stream on reset.
				continue
			}

			// Cancel any ongoing reads/writes.
			msch.cancelRead(ErrStreamReset)
			msch.cancelWrite(ErrStreamReset)
		case closeTag:
			if !ok {
				// may have canceled our reads already.
				continue
			}

			// unregister and throw away future data.
			mp.chLock.Lock()
			delete(mp.channels, ch)
			mp.chLock.Unlock()

			// close data channel, there will be no more data.
			close(msch.dataIn)

			// We intentionally don't cancel any deadlines, cancel reads, cancel
			// writes, etc. We just deliver the EOF by closing the
			// data channel, and unregister the channel so we don't
			// receive any more data. The user still needs to call
			// `Close()` or `Reset()`.
		case messageTag:
			if !ok {
				// We're not accepting data on this stream, for
				// some reason. It's likely that we reset it, or
				// simply canceled reads (e.g., called Close).
				mp.putBufferInbound(b)
				continue
			}

			recvTimeout.Reset(ReceiveTimeout)
			select {
			case msch.dataIn <- b:
			case <-msch.readCancel:
				// the user has canceled reading. walk away.
				mp.putBufferInbound(b)
			case <-recvTimeout.C:
				mp.putBufferInbound(b)
				log.Warnf("timed out receiving message into stream queue.")
				// Do not do this asynchronously. Otherwise, we
				// could drop a message, then receive a message,
				// then reset.
				msch.Reset()
				continue
			case <-mp.shutdown:
				mp.putBufferInbound(b)
				return
			}
			if !recvTimeout.Stop() {
				<-recvTimeout.C
			}
		default:
			log.Debugf("message with unknown header on stream %s", ch)
			if ok {
				msch.Reset()
			}
		}
	}
}

func (mp *Multiplex) isShutdown() bool {
	select {
	case <-mp.shutdown:
		return true
	default:
		return false
	}
}

func (mp *Multiplex) sendResetMsg(header uint64, hard bool) {
	ctx, cancel := context.WithTimeout(context.Background(), ResetStreamTimeout)
	defer cancel()

	err := mp.sendMsg(ctx.Done(), nil, header, nil)
	if err != nil && !mp.isShutdown() {
		if hard {
			log.Warnf("error sending reset message: %s; killing connection", err.Error())
			mp.Close()
		} else {
			log.Debugf("error sending reset message: %s", err.Error())
		}
	}
}

func (mp *Multiplex) readNextHeader() (uint64, uint64, error) {
	h, err := varint.ReadUvarint(mp.buf)
	if err != nil {
		return 0, 0, err
	}

	// get channel ID
	ch := h >> 3

	rem := h & 7

	return ch, rem, nil
}

func (mp *Multiplex) readNext() ([]byte, error) {
	// get length
	l, err := varint.ReadUvarint(mp.buf)
	if err != nil {
		return nil, err
	}

	if l > uint64(MaxMessageSize) {
		return nil, fmt.Errorf("message size too large")
	}

	if l == 0 {
		return nil, nil
	}

	buf, err := mp.getBufferInbound(int(l))
	if err != nil {
		return nil, err
	}
	n, err := io.ReadFull(mp.buf, buf)
	if err != nil {
		return nil, err
	}

	return buf[:n], nil
}

func (mp *Multiplex) getBufferInbound(length int) ([]byte, error) {
	select {
	case mp.bufIn <- struct{}{}:
	case <-mp.shutdown:
		return nil, ErrShutdown
	}

	return mp.getBuffer(length), nil
}

func (mp *Multiplex) getBufferOutbound(length int, timeout, cancel <-chan struct{}) ([]byte, error) {
	select {
	case mp.bufOut <- struct{}{}:
	case <-timeout:
		return nil, errTimeout
	case <-cancel:
		return nil, ErrStreamClosed
	case <-mp.shutdown:
		return nil, ErrShutdown
	}

	return mp.getBuffer(length), nil
}

func (mp *Multiplex) getBuffer(length int) []byte {
	return pool.Get(length)
}

func (mp *Multiplex) putBufferInbound(b []byte) {
	mp.putBuffer(b, mp.bufIn)
}

func (mp *Multiplex) putBufferOutbound(b []byte) {
	mp.putBuffer(b, mp.bufOut)
}

func (mp *Multiplex) putBuffer(slice []byte, putBuf chan struct{}) {
	<-putBuf
	pool.Put(slice)
}
