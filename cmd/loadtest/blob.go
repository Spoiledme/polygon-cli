package loadtest

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	_ "embed"
	"fmt"
	"math/rand"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/params"

	"github.com/ethereum/go-ethereum/core/types"
)

type BlobCommitment struct {
	Blob          kzg4844.Blob
	Commitment    kzg4844.Commitment
	Proof         kzg4844.Proof
	VersionedHash common.Hash
}

func encodeBlobData(data []byte) kzg4844.Blob {
	blob := kzg4844.Blob{}
	fieldIndex := -1
	for i := 0; i < len(data); i += 31 {
		fieldIndex++
		if fieldIndex == params.BlobTxFieldElementsPerBlob {
			break
		}
		max := i + 31
		if max > len(data) {
			max = len(data)
		}
		copy(blob[fieldIndex*32+1:], data[i:max])
	}
	return blob
}

func EncodeBlob(data []byte) (*BlobCommitment, error) {
	dataLen := len(data)
	if dataLen > params.BlobTxFieldElementsPerBlob*(params.BlobTxBytesPerFieldElement-1) {
		return nil, fmt.Errorf("Blob data longer than allowed (length: %v, limit: %v)", dataLen, params.BlobTxFieldElementsPerBlob*(params.BlobTxBytesPerFieldElement-1))
	}
	blobCommitment := BlobCommitment{
		Blob: encodeBlobData(data),
	}
	var err error

	// generate blob commitment
	blobCommitment.Commitment, err = kzg4844.BlobToCommitment(blobCommitment.Blob)
	if err != nil {
		return nil, fmt.Errorf("Failed generating blob commitment: %w", err)
	}

	// generate blob proof
	blobCommitment.Proof, err = kzg4844.ComputeBlobProof(blobCommitment.Blob, blobCommitment.Commitment)
	if err != nil {
		return nil, fmt.Errorf("Failed generating blob proof: %w", err)
	}

	// build versioned hash
	blobCommitment.VersionedHash = sha256.Sum256(blobCommitment.Commitment[:])
	blobCommitment.VersionedHash[0] = 0x01
	return &blobCommitment, nil
}

func parseBlobRefs(tx *types.BlobTx) error {
	var err error
	var blobBytes []byte
	var blobRefBytes []byte
	blobLen := rand.Intn((params.BlobTxFieldElementsPerBlob * (params.BlobTxBytesPerFieldElement - 1)) - len(blobBytes))
	blobRefBytes, _ = randomBlobData(blobLen)

	if blobRefBytes == nil {
		return fmt.Errorf("Unknown blob ref")
	}
	blobBytes = append(blobBytes, blobRefBytes...)

	blobCommitment, err := EncodeBlob(blobBytes)
	if err != nil {
		return fmt.Errorf("Invalid blob: %w", err)
	}

	tx.BlobHashes = append(tx.BlobHashes, blobCommitment.VersionedHash)
	tx.Sidecar.Blobs = append(tx.Sidecar.Blobs, blobCommitment.Blob)
	tx.Sidecar.Commitments = append(tx.Sidecar.Commitments, blobCommitment.Commitment)
	tx.Sidecar.Proofs = append(tx.Sidecar.Proofs, blobCommitment.Proof)
	return nil
}

func randomBlobData(size int) ([]byte, error) {
	data := make([]byte, size)
	n, err := cryptorand.Read(data)
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, fmt.Errorf("Could not create random blob data with size %d: %v", size, err)
	}
	return data, nil
}
