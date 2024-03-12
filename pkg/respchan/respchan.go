package respchan

import (
	"errors"
	"fmt"
	"sync"

	"github.com/ElecTwix/surrealdb-custom.go/internal/rpc"
	"github.com/ElecTwix/surrealdb-custom.go/pkg/model"
)

var ErrIDInUse = errors.New("id already in use")

type ResponseChannel[T rpc.RPCResponse | model.Notification] struct {
	responseChannels     map[string]chan T
	responseChannelsLock sync.RWMutex
}

func New[T rpc.RPCResponse | model.Notification]() *ResponseChannel[T] {
	return &ResponseChannel[T]{
		responseChannels: make(map[string]chan T),
	}
}

func (respChan *ResponseChannel[T]) CreateResponseChannel(id string) (chan T, error) {
	respChan.responseChannelsLock.Lock()
	defer respChan.responseChannelsLock.Unlock()

	if _, ok := respChan.responseChannels[id]; ok {
		return nil, fmt.Errorf("%w: %v", ErrIDInUse, id)
	}

	ch := make(chan T)
	respChan.responseChannels[id] = ch

	return ch, nil
}

func (ws *ResponseChannel[T]) RemoveResponseChannel(id string) {
	ws.responseChannelsLock.Lock()
	defer ws.responseChannelsLock.Unlock()
	delete(ws.responseChannels, id)
}

func (ws *ResponseChannel[T]) GetResponseChannel(id string) (chan T, bool) {
	ws.responseChannelsLock.RLock()
	defer ws.responseChannelsLock.RUnlock()
	ch, ok := ws.responseChannels[id]
	return ch, ok
}
