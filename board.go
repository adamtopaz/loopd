package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	boardKeep    = 500  // messages the board keeps
	boardShow    = 50   // messages `loopd board --all` shows
	messageLimit = 4000 // characters in one message
)

// message is one post on the board.
type message struct {
	ID   int       `json:"id"`
	Time time.Time `json:"time"`
	From string    `json:"from"`
	To   string    `json:"to,omitempty"`
	Text string    `json:"text"`
}

// board is a message board for the workers and the people running them. It keeps the latest
// messages, and the last message each worker has read, in a file, so that both survive a restart.
type board struct {
	mu    sync.Mutex
	path  string
	state struct {
		NextID   int            `json:"next_id"`
		Messages []message      `json:"messages"`
		Read     map[string]int `json:"read"`
	}
}

func openBoard(path string) (*board, error) {
	b := &board{path: path}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &b.state); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	if b.state.Read == nil {
		b.state.Read = map[string]int{}
	}
	b.state.NextID = max(b.state.NextID, 1)
	return b, nil
}

// save writes the board to its file, replacing the old one in one step. The caller holds b.mu.
func (b *board) save() error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(b.state)
	if err != nil {
		return err
	}
	temp := b.path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, b.path)
}

func (b *board) post(from, to, text string) (message, error) {
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return message{}, errors.New("the message is empty")
	case len([]rune(text)) > messageLimit:
		return message{}, fmt.Errorf("the message is longer than %d characters", messageLimit)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	m := message{ID: b.state.NextID, Time: time.Now(), From: from, To: to, Text: text}
	b.state.NextID++
	b.state.Messages = append(b.state.Messages, m)
	if extra := len(b.state.Messages) - boardKeep; extra > 0 {
		b.state.Messages = append([]message{}, b.state.Messages[extra:]...)
	}
	return m, b.save()
}

// unread returns the messages for worker that it hasn't read yet, leaving out its own posts and
// messages to other workers, and marks the board read up to now for it.
func (b *board) unread(worker string) ([]message, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	messages := []message{}
	for _, m := range b.state.Messages {
		if m.ID > b.state.Read[worker] && m.From != worker && (m.To == "" || m.To == worker) {
			messages = append(messages, m)
		}
	}
	if b.state.NextID-1 > b.state.Read[worker] {
		b.state.Read[worker] = b.state.NextID - 1
		return messages, b.save()
	}
	return messages, nil
}

// recent returns the latest n messages, whoever they are for.
func (b *board) recent(n int) []message {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := max(len(b.state.Messages)-n, 0)
	return append([]message{}, b.state.Messages[start:]...)
}
