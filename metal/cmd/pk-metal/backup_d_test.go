package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	osv1alpha1 "github.com/dennisklein/platform-kit/metal/api/os/v1alpha1"
)

func TestRecipientTails(t *testing.T) {
	assert.Equal(t, "…7k2q9xzw,…aaaabbbb", recipientTails([]string{
		"age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p7k2q9xzw",
		"age1zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzaaaabbbb",
	}))
	assert.Equal(t, "short", recipientTails([]string{"short"}), "a value shorter than the tail is shown whole")
	assert.Empty(t, recipientTails(nil))
}

// A download is kept only when its size and sha256 are those of the record.
func TestSaveCiphertextChecksTheRecord(t *testing.T) {
	stored := []byte("ciphertext as stored")
	sum := sha256.Sum256(stored)
	s := &osv1alpha1.EtcdSnapshot{Status: osv1alpha1.EtcdSnapshotStatus{
		EncryptedSize: int64(len(stored)), EncryptedSHA256: "sha256:" + hex.EncodeToString(sum[:]),
	}}
	var w bytes.Buffer

	good := filepath.Join(t.TempDir(), "good.db.age")
	require.NoError(t, saveCiphertext(s, bytes.NewReader(stored), good, &w))
	got, err := os.ReadFile(good)
	require.NoError(t, err)
	assert.Equal(t, stored, got)

	for name, body := range map[string][]byte{
		"another size":          []byte("short"),
		"same size, other data": []byte("ciphertext as STORED"),
	} {
		bad := filepath.Join(t.TempDir(), "bad.db.age")
		require.Error(t, saveCiphertext(s, bytes.NewReader(body), bad, &w), name)
		_, err := os.Stat(bad)
		assert.True(t, os.IsNotExist(err), "%s: the file is removed", name)
	}
}
