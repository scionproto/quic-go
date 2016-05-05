package quic

import (
	"io"
	"sync"
	"sync/atomic"

	"github.com/lucas-clemente/quic-go/frames"
	"github.com/lucas-clemente/quic-go/handshake"
	"github.com/lucas-clemente/quic-go/protocol"
	"github.com/lucas-clemente/quic-go/utils"
)

type streamHandler interface {
	QueueStreamFrame(*frames.StreamFrame) error
}

// A Stream assembles the data from StreamFrames and provides a super-convenient Read-Interface
type stream struct {
	session  streamHandler
	streamID protocol.StreamID
	// The chan of unordered stream frames. A nil in this channel is sent by the
	// session if an error occurred, in this case, remoteErr is filled before.
	streamFrames   chan *frames.StreamFrame
	currentFrame   *frames.StreamFrame
	readPosInFrame int
	writeOffset    uint64
	readOffset     uint64
	frameQueue     []*frames.StreamFrame // TODO: replace with heap
	remoteErr      error
	currentErr     error

	connectionParameterManager *handshake.ConnectionParametersManager

	flowControlWindow uint64
	windowUpdateCond  *sync.Cond
}

// newStream creates a new Stream
func newStream(session streamHandler, connectionParameterManager *handshake.ConnectionParametersManager, StreamID protocol.StreamID) (*stream, error) {
	s := &stream{
		session:                    session,
		streamID:                   StreamID,
		streamFrames:               make(chan *frames.StreamFrame, 8), // ToDo: add config option for this number
		connectionParameterManager: connectionParameterManager,
		windowUpdateCond:           sync.NewCond(&sync.Mutex{}),
	}

	flowControlWindow, err := connectionParameterManager.GetStreamFlowControlWindow()
	if err != nil {
		return nil, err
	}
	s.flowControlWindow = uint64(flowControlWindow)
	return s, nil
}

// Read implements io.Reader
func (s *stream) Read(p []byte) (int, error) {
	if s.currentErr != nil {
		return 0, s.currentErr
	}
	bytesRead := 0
	for bytesRead < len(p) {
		if s.currentFrame == nil {
			var err error
			s.currentFrame, err = s.getNextFrameInOrder(bytesRead == 0)
			if err != nil {
				s.currentErr = err
				return bytesRead, err
			}
			if s.currentFrame == nil {
				return bytesRead, nil
			}
			s.readPosInFrame = 0
		}
		m := utils.Min(len(p)-bytesRead, len(s.currentFrame.Data)-s.readPosInFrame)
		copy(p[bytesRead:], s.currentFrame.Data[s.readPosInFrame:])
		s.readPosInFrame += m
		bytesRead += m
		s.readOffset += uint64(m)
		if s.readPosInFrame >= len(s.currentFrame.Data) {
			fin := s.currentFrame.FinBit
			s.currentFrame = nil
			if fin {
				s.currentErr = io.EOF
				return bytesRead, io.EOF
			}
		}
	}

	return bytesRead, nil
}

func (s *stream) getNextFrameInOrder(wait bool) (*frames.StreamFrame, error) {
	// First, check the queue
	for i, f := range s.frameQueue {
		if f.Offset == s.readOffset {
			// Move last element into position i
			s.frameQueue[i] = s.frameQueue[len(s.frameQueue)-1]
			s.frameQueue = s.frameQueue[:len(s.frameQueue)-1]
			return f, nil
		}
	}

	for {
		nextFrameFromChannel, err := s.nextFrameInChan(wait)
		if err != nil {
			return nil, err
		}
		if nextFrameFromChannel == nil {
			return nil, nil
		}

		if nextFrameFromChannel.Offset == s.readOffset {
			return nextFrameFromChannel, nil
		}

		// Discard if we already know it
		if nextFrameFromChannel.Offset < s.readOffset {
			continue
		}

		// Append to queue
		s.frameQueue = append(s.frameQueue, nextFrameFromChannel)
	}
}

func (s *stream) nextFrameInChan(blocking bool) (*frames.StreamFrame, error) {
	var f *frames.StreamFrame
	var ok bool
	if blocking {
		f, ok = <-s.streamFrames
	} else {
		select {
		case f, ok = <-s.streamFrames:
		default:
			return nil, nil
		}
	}
	if !ok {
		panic("Stream: internal inconsistency: encountered closed chan without nil value (remote error) or FIN bit")
	}
	if f == nil {
		// We read nil, which indicates a remoteErr
		return nil, s.remoteErr
	}
	return f, nil
}

// ReadByte implements io.ByteReader
func (s *stream) ReadByte() (byte, error) {
	// TODO: Optimize
	p := make([]byte, 1)
	_, err := io.ReadFull(s, p)
	return p[0], err
}

func (s *stream) UpdateFlowControlWindow(n uint64) {
	if n > s.flowControlWindow {
		atomic.StoreUint64((*uint64)(&s.flowControlWindow), n)
		s.windowUpdateCond.Broadcast()
	}
}

func (s *stream) Write(p []byte) (int, error) {
	if s.remoteErr != nil {
		return 0, s.remoteErr
	}

	dataWritten := 0

	for dataWritten < len(p) {
		s.windowUpdateCond.L.Lock()
		remainingBytesInWindow := int64(s.flowControlWindow) - int64(s.writeOffset)
		for ; remainingBytesInWindow == 0; remainingBytesInWindow = int64(s.flowControlWindow) - int64(s.writeOffset) {
			if s.remoteErr != nil {
				return 0, s.remoteErr
			}
			s.windowUpdateCond.Wait()
		}
		s.windowUpdateCond.L.Unlock()

		dataLen := utils.Min(len(p), int(remainingBytesInWindow))
		data := make([]byte, dataLen)
		copy(data, p)
		err := s.session.QueueStreamFrame(&frames.StreamFrame{
			StreamID: s.streamID,
			Offset:   s.writeOffset,
			Data:     data,
		})
		if err != nil {
			return 0, err
		}

		dataWritten += dataLen
		s.writeOffset += uint64(dataLen)
	}

	return len(p), nil
}

// Close implements io.Closer
func (s *stream) Close() error {
	return s.session.QueueStreamFrame(&frames.StreamFrame{
		StreamID: s.streamID,
		Offset:   s.writeOffset,
		FinBit:   true,
	})
}

// AddStreamFrame adds a new stream frame
func (s *stream) AddStreamFrame(frame *frames.StreamFrame) error {
	s.streamFrames <- frame
	return nil
}

// RegisterError is called by session to indicate that an error occurred and the
// stream should be closed.
func (s *stream) RegisterError(err error) {
	s.remoteErr = err
	s.streamFrames <- nil
	s.windowUpdateCond.Broadcast()
}

func (s *stream) finishedReading() bool {
	return s.currentErr != nil
}
