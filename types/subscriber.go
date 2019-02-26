package types

import (
	"fmt"
)

func SubscriberRPCRequest() RPCRequest {
	req := NewRPCRequest().
		WithJSONRPC("2.0").
		WithID("0").
		WithMethod("subscribe")

	return req
}

func SubscriberRPCRequestWithQuery(query string) RPCRequest {
	req := SubscriberRPCRequest().WithQuery(query)

	return req
}

func NewTxSubscriberRPCRequest(txHash string) RPCRequest {
	query := fmt.Sprintf("tm.event = 'Tx' AND tx.hash = '%s'", txHash)
	req := SubscriberRPCRequestWithQuery(query)

	return req
}