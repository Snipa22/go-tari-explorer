package txsearch

import (
	"context"
	"encoding/hex"
	"errors"
	"net"
	"testing"

	"github.com/Snipa22/go-tari-grpc-lib/v3/tari_generated"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/Snipa22/go-tari-explorer/internal/db"
	"github.com/Snipa22/go-tari-explorer/internal/nodeclient"
)

// ---- fakeDB: a DBSearcher backed by an in-memory map, no real Postgres needed. ----

type fakeDB struct {
	kernelsBySig     map[string]db.Kernel // key: hex(signature scalar)
	kernelsByFullSig map[string]db.Kernel // key: hex(nonce)+hex(signature)
	outputsByComm    map[string]db.Output // key: hex(commitment)
}

func newFakeDB() *fakeDB {
	return &fakeDB{
		kernelsBySig:     map[string]db.Kernel{},
		kernelsByFullSig: map[string]db.Kernel{},
		outputsByComm:    map[string]db.Output{},
	}
}

func (f *fakeDB) FindKernelByExcessSigSignature(ctx context.Context, sig []byte) (db.Kernel, error) {
	if k, ok := f.kernelsBySig[hex.EncodeToString(sig)]; ok {
		return k, nil
	}
	return db.Kernel{}, pgx.ErrNoRows
}

func (f *fakeDB) FindKernelByExcessSig(ctx context.Context, nonce, sig []byte) (db.Kernel, error) {
	key := hex.EncodeToString(nonce) + hex.EncodeToString(sig)
	if k, ok := f.kernelsByFullSig[key]; ok {
		return k, nil
	}
	return db.Kernel{}, pgx.ErrNoRows
}

func (f *fakeDB) FindOutputByCommitment(ctx context.Context, commitment []byte) (db.Output, error) {
	if o, ok := f.outputsByComm[hex.EncodeToString(commitment)]; ok {
		return o, nil
	}
	return db.Output{}, pgx.ErrNoRows
}

// ---- fakeBaseNodeServer: a real in-process GRPC BaseNode server (bufconn), so the
// NodeSearcher-path tests exercise the actual wire calls through a real
// *nodeclient.Client, not a hand-rolled NodeSearcher stub. ----

type fakeBaseNodeServer struct {
	tari_generated.UnimplementedBaseNodeServer

	kernelBlocks map[string]*tari_generated.HistoricalBlock // key: hex(nonce)+hex(sig)
	utxoBlocks   map[string]*tari_generated.HistoricalBlock // key: hex(commitment)
	paymentRefs  map[string]*tari_generated.PaymentReferenceResponse
	txStates     map[string]tari_generated.TransactionLocation // key: hex(nonce)+hex(sig)
}

func newFakeBaseNodeServer() *fakeBaseNodeServer {
	return &fakeBaseNodeServer{
		kernelBlocks: map[string]*tari_generated.HistoricalBlock{},
		utxoBlocks:   map[string]*tari_generated.HistoricalBlock{},
		paymentRefs:  map[string]*tari_generated.PaymentReferenceResponse{},
		txStates:     map[string]tari_generated.TransactionLocation{},
	}
}

func (f *fakeBaseNodeServer) SearchKernels(req *tari_generated.SearchKernelsRequest, stream tari_generated.BaseNode_SearchKernelsServer) error {
	for _, sig := range req.GetSignatures() {
		key := hex.EncodeToString(sig.GetPublicNonce()) + hex.EncodeToString(sig.GetSignature())
		if block, ok := f.kernelBlocks[key]; ok {
			if err := stream.Send(block); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) SearchUtxos(req *tari_generated.SearchUtxosRequest, stream tari_generated.BaseNode_SearchUtxosServer) error {
	for _, c := range req.GetCommitments() {
		if block, ok := f.utxoBlocks[hex.EncodeToString(c)]; ok {
			if err := stream.Send(block); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) SearchPaymentReferences(req *tari_generated.SearchPaymentReferencesRequest, stream tari_generated.BaseNode_SearchPaymentReferencesServer) error {
	for _, ref := range req.GetPaymentReferenceHex() {
		if resp, ok := f.paymentRefs[ref]; ok {
			if err := stream.Send(resp); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *fakeBaseNodeServer) TransactionState(ctx context.Context, req *tari_generated.TransactionStateRequest) (*tari_generated.TransactionStateResponse, error) {
	sig := req.GetExcessSig()
	key := hex.EncodeToString(sig.GetPublicNonce()) + hex.EncodeToString(sig.GetSignature())
	state, ok := f.txStates[key]
	if !ok {
		state = tari_generated.TransactionLocation_NOT_STORED
	}
	return &tari_generated.TransactionStateResponse{Result: state}, nil
}

// startFakeServer boots fake on a bufconn listener and returns a real *nodeclient.Client
// dialed against it (via nodeclient.New's opts... seam - see nodeclient.go), plus a
// cleanup func. This exercises internal/nodeclient's actual dial/withFailover/stream-
// draining code, not just txsearch's own logic, giving genuine wire-level coverage.
func startFakeServer(t *testing.T, fake *fakeBaseNodeServer) (*nodeclient.Client, func()) {
	t.Helper()

	lis := bufconn.Listen(1024 * 1024)
	grpcServer := grpc.NewServer()
	tari_generated.RegisterBaseNodeServer(grpcServer, fake)
	go func() { _ = grpcServer.Serve(lis) }()

	dialer := grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return lis.DialContext(ctx)
	})

	client, err := nodeclient.New([]string{"passthrough:///bufnet"}, dialer, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("nodeclient.New: %v", err)
	}

	cleanup := func() {
		_ = client.Close()
		grpcServer.Stop()
		_ = lis.Close()
	}
	return client, cleanup
}

func hexBytes(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

// ---- Indexed-path tests (fakeDB only, no GRPC involved). ----

func TestSearch_IndexedKernelBySignatureScalar_32Byte(t *testing.T) {
	fdb := newFakeDB()
	sig := hexBytes(32, 0x11)
	fdb.kernelsBySig[hex.EncodeToString(sig)] = db.Kernel{BlockHeight: 42, Index: 3, Fee: 1000}

	s := New(fdb, nil)
	result, err := s.Search(context.Background(), hex.EncodeToString(sig))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindKernel || result.Source != SourceIndexed || result.BlockHeight != 42 {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.Kernel == nil || result.Kernel.Fee != 1000 {
		t.Fatalf("expected kernel with fee 1000, got %+v", result.Kernel)
	}
}

func TestSearch_IndexedOutputByCommitment_32Byte(t *testing.T) {
	fdb := newFakeDB()
	commitment := hexBytes(32, 0x22)
	fdb.outputsByComm[hex.EncodeToString(commitment)] = db.Output{BlockHeight: 55, Index: 1, OutputType: 1}

	s := New(fdb, nil)
	result, err := s.Search(context.Background(), "0x"+hex.EncodeToString(commitment))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindOutput || result.Source != SourceIndexed || result.BlockHeight != 55 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSearch_IndexedKernelByFullSig_64Byte(t *testing.T) {
	fdb := newFakeDB()
	nonce := hexBytes(32, 0x33)
	sig := hexBytes(32, 0x44)
	fdb.kernelsByFullSig[hex.EncodeToString(nonce)+hex.EncodeToString(sig)] = db.Kernel{BlockHeight: 77, Fee: 55}

	s := New(fdb, nil)
	query := hex.EncodeToString(nonce) + hex.EncodeToString(sig)
	result, err := s.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindKernel || result.BlockHeight != 77 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSearch_NonHexInput_ReportsNotFoundNotError(t *testing.T) {
	s := New(newFakeDB(), nil)
	result, err := s.Search(context.Background(), "not-hex-garbage!!")
	if err != nil {
		t.Fatalf("expected no error for garbage input, got %v", err)
	}
	if result.Found {
		t.Fatalf("expected not-found, got %+v", result)
	}
}

func TestSearch_WrongLength_ReportsNotFound(t *testing.T) {
	s := New(newFakeDB(), nil)
	// 16 bytes hex-encoded = 32 hex chars, neither 32 nor 64 bytes.
	result, err := s.Search(context.Background(), hex.EncodeToString(hexBytes(16, 0x01)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatalf("expected not-found for unsupported length, got %+v", result)
	}
}

func TestSearch_NilDBAndNode_ReportsNotFound(t *testing.T) {
	s := New(nil, nil)
	result, err := s.Search(context.Background(), hex.EncodeToString(hexBytes(32, 0x01)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatalf("expected not-found with no DB/node configured, got %+v", result)
	}
}

// ---- Live-GRPC-path tests: real bufconn server + real *nodeclient.Client, wrapped in
// txsearch.Searcher with a fakeDB that always misses (forcing the live fallback). ----

func TestSearch_LiveKernelFallback_64Byte(t *testing.T) {
	fake := newFakeBaseNodeServer()
	nonce := hexBytes(32, 0x55)
	sig := hexBytes(32, 0x66)
	wantHash := hexBytes(32, 0x77)
	fake.kernelBlocks[hex.EncodeToString(nonce)+hex.EncodeToString(sig)] = &tari_generated.HistoricalBlock{
		Block: &tari_generated.Block{
			Header: &tari_generated.BlockHeader{Height: 900, Hash: wantHash},
		},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client) // fakeDB always misses -> forces live path
	query := hex.EncodeToString(nonce) + hex.EncodeToString(sig)
	result, err := s.Search(context.Background(), query)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindKernel || result.Source != SourceLive {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.BlockHeight != 900 {
		t.Fatalf("expected block height 900, got %d", result.BlockHeight)
	}
	if result.BlockHash != hex.EncodeToString(wantHash) {
		t.Fatalf("expected block hash %x, got %s", wantHash, result.BlockHash)
	}
}

func TestSearch_LiveUtxoFallback_32Byte(t *testing.T) {
	fake := newFakeBaseNodeServer()
	commitment := hexBytes(32, 0x88)
	fake.utxoBlocks[hex.EncodeToString(commitment)] = &tari_generated.HistoricalBlock{
		Block: &tari_generated.Block{Header: &tari_generated.BlockHeader{Height: 111}},
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	result, err := s.Search(context.Background(), hex.EncodeToString(commitment))
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindOutput || result.Source != SourceLive || result.BlockHeight != 111 {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestSearch_LivePaymentReferenceFallback_32Byte(t *testing.T) {
	fake := newFakeBaseNodeServer()
	ref := hexBytes(32, 0x99)
	refHex := hex.EncodeToString(ref)
	fake.paymentRefs[refHex] = &tari_generated.PaymentReferenceResponse{
		BlockHeight: 222,
		BlockHash:   hexBytes(8, 0xAA),
		IsSpent:     true,
	}
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	result, err := s.Search(context.Background(), refHex)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !result.Found || result.Kind != KindPaymentReference || result.Source != SourceLive {
		t.Fatalf("unexpected result: %+v", result)
	}
	if result.BlockHeight != 222 || !result.PaymentReference.GetIsSpent() {
		t.Fatalf("unexpected payment reference result: %+v", result)
	}
}

func TestSearch_NothingMatchesAnywhere(t *testing.T) {
	fake := newFakeBaseNodeServer() // empty: nothing configured to match
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	result, err := s.Search(context.Background(), hex.EncodeToString(hexBytes(32, 0xEE)))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Found {
		t.Fatalf("expected not-found, got %+v", result)
	}
}

// ---- CheckTransactionState tests: always live, via the real bufconn server. ----

func TestCheckTransactionState_Mempool(t *testing.T) {
	fake := newFakeBaseNodeServer()
	nonce := hexBytes(32, 0x01)
	sig := hexBytes(32, 0x02)
	fake.txStates[hex.EncodeToString(nonce)+hex.EncodeToString(sig)] = tari_generated.TransactionLocation_MEMPOOL

	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	result, err := s.CheckTransactionState(context.Background(), hex.EncodeToString(nonce)+hex.EncodeToString(sig))
	if err != nil {
		t.Fatalf("CheckTransactionState: %v", err)
	}
	if result.State != "MEMPOOL" {
		t.Fatalf("expected MEMPOOL, got %q", result.State)
	}
}

func TestCheckTransactionState_Mined(t *testing.T) {
	fake := newFakeBaseNodeServer()
	nonce := hexBytes(32, 0x03)
	sig := hexBytes(32, 0x04)
	fake.txStates[hex.EncodeToString(nonce)+hex.EncodeToString(sig)] = tari_generated.TransactionLocation_MINED

	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	result, err := s.CheckTransactionState(context.Background(), hex.EncodeToString(nonce)+hex.EncodeToString(sig))
	if err != nil {
		t.Fatalf("CheckTransactionState: %v", err)
	}
	if result.State != "MINED" {
		t.Fatalf("expected MINED, got %q", result.State)
	}
}

func TestCheckTransactionState_NotStoredDefault(t *testing.T) {
	fake := newFakeBaseNodeServer() // no txStates configured -> server defaults to NOT_STORED
	client, cleanup := startFakeServer(t, fake)
	defer cleanup()

	s := New(newFakeDB(), client)
	query := hex.EncodeToString(hexBytes(32, 0x05)) + hex.EncodeToString(hexBytes(32, 0x06))
	result, err := s.CheckTransactionState(context.Background(), query)
	if err != nil {
		t.Fatalf("CheckTransactionState: %v", err)
	}
	if result.State != "NOT_STORED" {
		t.Fatalf("expected NOT_STORED, got %q", result.State)
	}
}

func TestCheckTransactionState_InvalidHex(t *testing.T) {
	s := New(newFakeDB(), nil)
	if _, err := s.CheckTransactionState(context.Background(), "zz-not-hex"); err == nil {
		t.Fatal("expected error for non-hex excess_sig")
	}
}

func TestCheckTransactionState_WrongLength(t *testing.T) {
	s := New(newFakeDB(), nil)
	if _, err := s.CheckTransactionState(context.Background(), hex.EncodeToString(hexBytes(32, 0x01))); err == nil {
		t.Fatal("expected error for a 32-byte (not 64-byte) excess_sig")
	}
}

func TestCheckTransactionState_NoNodeConfigured(t *testing.T) {
	s := New(newFakeDB(), nil)
	query := hex.EncodeToString(hexBytes(32, 0x01)) + hex.EncodeToString(hexBytes(32, 0x02))
	if _, err := s.CheckTransactionState(context.Background(), query); err == nil {
		t.Fatal("expected error when no node client is configured")
	}
}

// TestSearch_DBErrorPropagates proves a genuine (non-ErrNoRows) DB error is surfaced
// as an error, not silently swallowed into a not-found result.
type erroringDB struct{}

func (erroringDB) FindKernelByExcessSigSignature(ctx context.Context, sig []byte) (db.Kernel, error) {
	return db.Kernel{}, errors.New("boom")
}
func (erroringDB) FindKernelByExcessSig(ctx context.Context, nonce, sig []byte) (db.Kernel, error) {
	return db.Kernel{}, errors.New("boom")
}
func (erroringDB) FindOutputByCommitment(ctx context.Context, commitment []byte) (db.Output, error) {
	return db.Output{}, errors.New("boom")
}

func TestSearch_DBErrorPropagates(t *testing.T) {
	s := New(erroringDB{}, nil)
	_, err := s.Search(context.Background(), hex.EncodeToString(hexBytes(32, 0x01)))
	if err == nil {
		t.Fatal("expected a genuine DB error to propagate, got nil")
	}
}
