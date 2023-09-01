package s3

import (
	"bytes"
	"fmt"
	"io"
	"sync"

	s3manager "github.com/aws/aws-sdk-go-v2/feature/s3/manager"
)

type DownloadEventType int

const (
	NewData DownloadEventType = iota
	DownloadComplete
	DownloadError
)

type DownloadEvent struct {
	Type DownloadEventType
	Data []byte
	Err  error
}

type Chunk struct {
	data []byte
	end  int64
}

type DownloadBuffer struct {
	writeOffset int64
	buffer      bytes.Buffer
	chunks      map[int64]Chunk
	events      chan DownloadEvent
	lock        sync.Mutex
	complete    bool
}

func NewDownloadBuffer() *DownloadBuffer {
	return &DownloadBuffer{
		chunks: make(map[int64]Chunk),
		events: make(chan DownloadEvent),
	}
}

func (b *DownloadBuffer) WriteAt(p []byte, pos int64) (n int, err error) {
	pLen := len(p)
	b.lock.Lock()
	defer b.lock.Unlock()
	b.chunks[pos] = Chunk{data: p, end: pos + int64(pLen)}
	for {
		chunk, ok := b.chunks[b.writeOffset]
		if !ok {
			break
		}
		b.events <- DownloadEvent{
			Type: NewData,
			Data: chunk.data,
		}
		delete(b.chunks, b.writeOffset)
		b.writeOffset = chunk.end
	}
	return pLen, nil
}

func (b *DownloadBuffer) Error(err error) {
	b.events <- DownloadEvent{Type: DownloadError, Err: err}
}

func (b *DownloadBuffer) End() {
	b.events <- DownloadEvent{Type: DownloadComplete}
}

func (b *DownloadBuffer) Read(p []byte) (n int, err error) {
	if b.complete {
		return 0, io.EOF
	}
	bytesRead := 0
	for {
		newBytes, err := io.ReadFull(&b.buffer, p[bytesRead:])
		bytesRead += newBytes
		if err == nil {
			return bytesRead, nil
		}
		if err != io.EOF && err != io.ErrUnexpectedEOF {
			return bytesRead, err
		}
		event := <-b.events
		switch event.Type {
		case NewData:
			b.buffer.Write(event.Data)
			continue
		case DownloadComplete:
			b.complete = true
			return bytesRead, nil
		case DownloadError:
			return bytesRead, event.Err
		}
	}
}

func (b *DownloadBuffer) Close() (err error) {
	if !b.complete {
		return fmt.Errorf("Can't cancel S3 Download")
	}
	close(b.events)
	return nil
}

type ReaderFrom struct {
	PartSize int64
	Writer   io.Writer
}

func (rf *ReaderFrom) Write(p []byte) (n int, err error) {
	return rf.Writer.Write(p)
}

func (rf *ReaderFrom) ReadFrom(r io.Reader) (n int64, err error) {
	buff := make([]byte, rf.PartSize)
	bytesRead := int64(0)
	for {
		newBytes, err := r.Read(buff[bytesRead:])
		bytesRead += int64(newBytes)
		if bytesRead > rf.PartSize {
			return 0, fmt.Errorf("Unexpected download size")
		}
		if bytesRead == rf.PartSize || err == io.EOF {
			break
		}
	}
	bytesWritten, err := rf.Writer.Write(buff[:bytesRead])
	if err != nil || int64(bytesWritten) != bytesRead {
		panic("DownloadBuffer failure")
	}
	return bytesRead, nil
}

type BufferProvider struct {
	PartSize int64
}

func (bp *BufferProvider) GetReadFrom(writer io.Writer) (w s3manager.WriterReadFrom, cleanup func()) {
	return &ReaderFrom{
		PartSize: bp.PartSize,
		Writer:   writer,
	}, func() {}
}
