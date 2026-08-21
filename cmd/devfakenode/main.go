// Command devfakenode runs a minimal fake BaseNode GRPC server (only TransactionState
// implemented) for manually exercising the /tx-state route against something that
// speaks the real wire protocol, without needing a live Tari base node. Dev/e2e tool
// only, not part of the production binary set.
package main

import (
	"context"
	"log"
	"net"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"google.golang.org/grpc"
)

type fakeServer struct {
	tari_generated.UnimplementedBaseNodeServer
}

func (f *fakeServer) TransactionState(ctx context.Context, req *tari_generated.TransactionStateRequest) (*tari_generated.TransactionStateResponse, error) {
	sig := req.GetExcessSig()
	log.Printf("TransactionState called for nonce=%x sig=%x", sig.GetPublicNonce(), sig.GetSignature())
	return &tari_generated.TransactionStateResponse{Result: tari_generated.TransactionLocation_MEMPOOL}, nil
}

func main() {
	lis, err := net.Listen("tcp", "127.0.0.1:18999")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	s := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(s, &fakeServer{})
	log.Println("fake base node grpc listening on 127.0.0.1:18999")
	if err := s.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
