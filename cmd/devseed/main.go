// Command devseed inserts a single known block + kernel + output row into a local
// Postgres for manual/e2e testing of the search and block-detail routes - not part of
// the production indexer path. Hardcoded DSN targets a throwaway local dev DB; not
// intended for CI or production use.
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/Snipa22/go-tari-explorer/internal/db"
)

func main() {
	ctx := context.Background()
	d, err := db.Connect(ctx, "postgres://postgres@localhost:5433/tari_explorer_txcheck_e2e?sslmode=disable&host=/workspace/pg-embed/sockets")
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	height := uint64(777)
	block := db.Block{
		Height:            height,
		Hash:              "deadbeef01",
		PrevHash:          "cafebabe02",
		Version:           1,
		Timestamp:         1_700_000_000,
		OutputMr:          []byte{},
		BlockOutputMr:     []byte{},
		KernelMr:          []byte{},
		InputMr:           []byte{},
		TotalKernelOffset: []byte{},
		TotalScriptOffset: []byte{},
		ValidatorNodeMr:   []byte{},
		PowData:           []byte{},
		PowAlgo:           "RXM",
		Difficulty:        123456,
		KernelCount:       2,
		OutputCount:       2,
	}
	if err := d.UpsertBlock(ctx, block); err != nil {
		log.Fatalf("upsert block: %v", err)
	}

	nonce := repeated(32, 0x11)
	sig := repeated(32, 0x22)
	kernels := []db.Kernel{
		{Index: 0, Features: 1, Fee: 5_000_000, ExcessSigNonce: nonce, ExcessSigSignature: sig, Excess: []byte{0xAA}},
		{Index: 1, Features: 0, Fee: 1_000, ExcessSigNonce: repeated(32, 0x33), ExcessSigSignature: repeated(32, 0x44)},
	}
	if err := d.ReplaceKernelsForBlock(ctx, height, kernels); err != nil {
		log.Fatalf("replace kernels: %v", err)
	}

	commitment := repeated(32, 0x55)
	outputs := []db.Output{
		{Index: 0, OutputType: 1, Maturity: 60, CoinbaseExtra: []byte("WUFJagtechE0"), Commitment: commitment},
		{Index: 1, OutputType: 0, Commitment: repeated(32, 0x66)},
	}
	if err := d.ReplaceOutputsForBlock(ctx, height, outputs); err != nil {
		log.Fatalf("replace outputs: %v", err)
	}

	fmt.Println("seeded block height:", height)
	fmt.Println("seeded kernel excess_sig (nonce+sig hex):", hexStr(nonce)+hexStr(sig))
	fmt.Println("seeded commitment hex:", hexStr(commitment))
}

func repeated(n int, fill byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = fill
	}
	return b
}

func hexStr(b []byte) string {
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexChars[c>>4]
		out[i*2+1] = hexChars[c&0x0f]
	}
	return string(out)
}
