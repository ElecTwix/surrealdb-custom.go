package nhooyr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"github.com/ElecTwix/surrealdb-custom.go/pkg/model"
	"github.com/ElecTwix/surrealdb-custom.go/pkg/respchan"
	nhooyr "nhooyr.io/websocket"

	"github.com/ElecTwix/surrealdb-custom.go/internal/rpc"
	"github.com/ElecTwix/surrealdb-custom.go/pkg/conn"
	"github.com/ElecTwix/surrealdb-custom.go/pkg/logger"
	"github.com/ElecTwix/surrealdb-custom.go/pkg/rand"
)

const (
	// RequestIDLength size of id sent on WS request
	RequestIDLength = 16
	// CloseMessageCode identifier the message id for a close request
	CloseMessageCode = 1000
	// DefaultTimeout timeout in seconds
	DefaultTimeout = 30
)

type Option func(ws *WebSocket) error

type WebSocket struct {
	Conn     *nhooyr.Conn
	connLock sync.Mutex
	Timeout  time.Duration
	Option   []Option
	logger   logger.Logger

	respChan       *respchan.ResponseChannel[rpc.RPCResponse]
	respNotifyChan *respchan.ResponseChannel[model.Notification]

	close chan int
}

func Create() *WebSocket {
	return &WebSocket{
		Conn:           nil,
		close:          make(chan int),
		respChan:       respchan.New[rpc.RPCResponse](),
		respNotifyChan: respchan.New[model.Notification](),
		Timeout:        DefaultTimeout * time.Second,
	}
}

func (ws *WebSocket) Connect(url string) (conn.Connection, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	connection, resp, err := nhooyr.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	ws.Conn = connection

	for _, option := range ws.Option {
		if err := option(ws); err != nil {
			return ws, err
		}
	}

	return ws, nil
}

func (ws *WebSocket) SetTimeOut(timeout time.Duration) *WebSocket {
	ws.Option = append(ws.Option, func(ws *WebSocket) error {
		ws.Timeout = timeout
		return nil
	})
	return ws
}

func (ws *WebSocket) SetCompression(compression bool) *WebSocket {
	ws.Option = append(ws.Option, func(ws *WebSocket) error {
		return nil
	})
	return ws
}

// If path is empty it will use os.stdout/os.stderr
func (ws *WebSocket) Logger(logData logger.Logger) *WebSocket {
	ws.logger = logData
	return ws
}

func (ws *WebSocket) RawLogger(logData logger.Logger) *WebSocket {
	ws.logger = logData
	return ws
}

func (ws *WebSocket) Close() error {
	ws.connLock.Lock()
	defer ws.connLock.Unlock()
	close(ws.close)

	return ws.Conn.Close(nhooyr.StatusNormalClosure, "")
}

func (ws *WebSocket) LiveNotifications(liveQueryID string) (chan model.Notification, error) {
	c, err := ws.respNotifyChan.CreateResponseChannel(liveQueryID)
	if err != nil {
		ws.logger.Error(err.Error())
	}
	return c, err
}

func (ws *WebSocket) Send(method string, params []interface{}) (interface{}, error) {
	id := rand.String(RequestIDLength)
	request := &rpc.RPCRequest{
		ID:     id,
		Method: method,
		Params: params,
	}

	responseChan, err := ws.respChan.CreateResponseChannel(id)
	if err != nil {
		return nil, err
	}
	defer ws.respChan.RemoveResponseChannel(id)

	if err := ws.write(request); err != nil {
		return nil, err
	}

	timeout := time.After(ws.Timeout)

	select {
	case <-timeout:
		return nil, conn.ErrTimeout
	case res, open := <-responseChan:
		if !open {
			return nil, errors.New("channel closed")
		}
		if res.ID != id {
			return nil, conn.ErrInvalidResponseID
		}
		if res.Error != nil {
			return nil, res.Error
		}
		return res.Result, nil
	}
}

func (ws *WebSocket) read(v interface{}) error {
	_, data, err := ws.Conn.Read(context.Background())
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func (ws *WebSocket) write(v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}

	ws.connLock.Lock()
	defer ws.connLock.Unlock()
	return ws.Conn.Write(context.Background(), nhooyr.MessageText, data)
}

func (ws *WebSocket) Start() error {
	for {
		select {
		case <-ws.close:
			return net.ErrClosed
		default:
			var res rpc.RPCResponse
			err := ws.read(&res)
			if err != nil {
				ws.logger.Error(err.Error())
				if errors.Is(err, net.ErrClosed) {
					return net.ErrClosed
				}
				continue
			}
			go ws.handleResponse(res)
		}
	}
}

func (ws *WebSocket) handleResponse(res rpc.RPCResponse) {
	if res.ID != nil && res.ID != "" {
		// Try to resolve message as response to query
		responseChan, ok := ws.respChan.GetResponseChannel(fmt.Sprintf("%v", res.ID))
		if !ok {
			err := fmt.Errorf("unavailable ResponseChannel %+v", res.ID)
			ws.logger.Error(err.Error())
			return
		}
		defer close(responseChan)
		responseChan <- res
	} else {
		// Try to resolve response as live query notification
		mappedRes, _ := res.Result.(map[string]interface{})
		resolvedID, ok := mappedRes["id"]
		if !ok {
			err := fmt.Errorf("response did not contain an 'id' field")

			ws.logger.Error(err.Error(), "result", fmt.Sprint(res.Result))
			return
		}
		var notification model.Notification
		err := unmarshalMapToStruct(mappedRes, &notification)
		if err != nil {
			ws.logger.Error(err.Error(), "result", fmt.Sprint(res.Result))
			return
		}
		LiveNotificationChan, ok := ws.respNotifyChan.GetResponseChannel(notification.ID)
		if !ok {
			err := fmt.Errorf("unavailable ResponseChannel %+v", resolvedID)
			ws.logger.Error(err.Error(), "result", fmt.Sprint(res.Result))
			return
		}
		LiveNotificationChan <- notification
	}
}

func unmarshalMapToStruct(data map[string]interface{}, outStruct interface{}) error {
	outValue := reflect.ValueOf(outStruct)
	if outValue.Kind() != reflect.Ptr || outValue.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("outStruct must be a pointer to a struct")
	}

	structValue := outValue.Elem()
	structType := structValue.Type()

	for i := 0; i < structValue.NumField(); i++ {
		field := structType.Field(i)
		fieldName := field.Name
		jsonTag := field.Tag.Get("json")
		if jsonTag != "" {
			fieldName = jsonTag
		}
		mapValue, ok := data[fieldName]
		if !ok {
			return fmt.Errorf("missing field in map: %s", fieldName)
		}

		fieldValue := structValue.Field(i)
		if !fieldValue.CanSet() {
			return fmt.Errorf("cannot set field: %s", fieldName)
		}

		if mapValue == nil {
			// Handle nil values appropriately for your struct fields
			// For simplicity, we skip nil values in this example
			continue
		}

		// Type conversion based on the field type
		switch fieldValue.Kind() {
		case reflect.String:
			fieldValue.SetString(fmt.Sprint(mapValue))
		case reflect.Int:
			intVal, err := strconv.Atoi(fmt.Sprint(mapValue))
			if err != nil {
				return err
			}
			fieldValue.SetInt(int64(intVal))
		case reflect.Bool:
			boolVal, err := strconv.ParseBool(fmt.Sprint(mapValue))
			if err != nil {
				return err
			}
			fieldValue.SetBool(boolVal)
		case reflect.Interface:
			fieldValue.Set(reflect.ValueOf(mapValue))
		// Add cases for other types as needed
		default:
			return fmt.Errorf("unsupported field type: %s", fieldName)
		}
	}

	return nil
}
