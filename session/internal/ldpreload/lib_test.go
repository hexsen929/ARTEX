package ldpreload

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"testing"
)

func TestBuildPreservesExistingProbeBytes(t *testing.T) {
	blob := Build()
	sum := sha256.Sum256(blob)
	const want = "8cfadc98cb347c1fccfc9d8211abc4e9a4210adf7d2650e209bc9f156c1cad24"
	if hex.EncodeToString(sum[:]) != want {
		t.Fatal("source-constructed probe differs from the imported marker-only fixture")
	}
	image, err := elf.NewFile(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer image.Close()
	if image.Machine != elf.EM_X86_64 || image.Type != elf.ET_DYN || image.Class != elf.ELFCLASS64 {
		t.Fatalf("unexpected probe architecture: %+v", image.FileHeader)
	}
}

func TestBuildReturnsIndependentImages(t *testing.T) {
	first, second := Build(), Build()
	if !bytes.Equal(first, second) {
		t.Fatal("non-deterministic images")
	}
	first[0] ^= 0xff
	if bytes.Equal(first, second) {
		t.Fatal("probe images share mutable storage")
	}
}
