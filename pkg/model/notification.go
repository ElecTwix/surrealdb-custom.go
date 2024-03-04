package model

import "github.com/surrealdb/surrealdb.go/internal/rpc"

type Notification struct {
	Action Action      `json:"action"`
	ID     string      `json:"id"`
	Result interface{} `json:"result"`
}
type Action string

const (
	CreateAction Action = "CREATE"
	UpdateAction Action = "UPDATE"
	DeleteAction Action = "DELETE"
)

func NewNotficton(resp rpc.RPCNotification) Notification {
	return Notification{
		ID:     resp.ID.(string),
		Action: Action(resp.Method),
		Result: resp.Params[0],
	}
}
