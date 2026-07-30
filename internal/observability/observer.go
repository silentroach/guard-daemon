package observability

import (
	"fmt"
	"io"
	"sync"

	"guard-daemon/internal/domain"

	"github.com/ethereum/go-ethereum/common"
)

type Level uint8

const (
	LevelInfo Level = iota + 1
	LevelWarning
	LevelError
)

type EventCode string

type Event struct {
	Level       Level
	Code        EventCode
	NetworkName string
	Candidate   domain.CandidateID
	TokenSymbol string
	Amount      string
	TxHash      common.Hash
	ErrorCode   domain.ErrorCode
}

type Observer interface {
	Record(Event)
}

type Discard struct{}

func (Discard) Record(Event) {}

type Console struct {
	mu     sync.Mutex
	writer io.Writer
}

func NewConsole(writer io.Writer) *Console {
	return &Console{writer: writer}
}

func (console *Console) Record(event Event) {
	console.mu.Lock()
	defer console.mu.Unlock()
	fmt.Fprintf(console.writer, "[%-9s] %s", event.NetworkName, event.Code)
	if event.TokenSymbol != "" {
		fmt.Fprintf(console.writer, " token=%s", event.TokenSymbol)
	}
	if event.Amount != "" {
		fmt.Fprintf(console.writer, " amount=%s", event.Amount)
	}
	if event.TxHash != (common.Hash{}) {
		fmt.Fprintf(console.writer, " tx=%s", event.TxHash.Hex())
	}
	if event.ErrorCode != "" {
		fmt.Fprintf(console.writer, " error=%s", event.ErrorCode)
	}
	fprintln(console.writer)
}

func fprintln(writer io.Writer) {
	_, _ = fmt.Fprintln(writer)
}
