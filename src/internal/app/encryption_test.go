package app

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestHKDFSHA256(t *testing.T) {
	// Verify deterministic output for a known input
	secret := make([]byte, 32)
	for i := range secret {
		secret[i] = byte(i)
	}

	key1, err := hkdfSHA256(secret)
	if err != nil {
		t.Fatalf("hkdfSHA256 failed: %v", err)
	}
	if len(key1) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(key1))
	}

	// Same input must produce same output
	key2, err := hkdfSHA256(secret)
	if err != nil {
		t.Fatalf("hkdfSHA256 failed on second call: %v", err)
	}
	if !bytes.Equal(key1, key2) {
		t.Fatal("hkdfSHA256 is not deterministic")
	}

	// Different input must produce different output
	secret[0] = 0xff
	key3, err := hkdfSHA256(secret)
	if err != nil {
		t.Fatalf("hkdfSHA256 failed with different input: %v", err)
	}
	if bytes.Equal(key1, key3) {
		t.Fatal("different inputs produced same key")
	}
}

func TestDefaultPublicKey(t *testing.T) {
	key, err := DefaultPublicKey()
	if err != nil {
		t.Fatalf("DefaultPublicKey failed: %v", err)
	}
	if key == nil {
		t.Fatal("DefaultPublicKey returned nil")
	}
	// Verify it's a 65-byte uncompressed P-256 point
	raw := key.Bytes()
	if len(raw) != 65 {
		t.Fatalf("expected 65 bytes, got %d", len(raw))
	}
	if raw[0] != 0x04 {
		t.Fatalf("expected 0x04 prefix, got 0x%02x", raw[0])
	}
}

func TestGenerateKeyPair(t *testing.T) {
	privPEM, pubB64, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	// Verify PEM is non-empty and parseable
	if len(privPEM) == 0 {
		t.Fatal("empty private key PEM")
	}

	// Write PEM to temp file and load it back
	tmpDir := t.TempDir()
	privPath := filepath.Join(tmpDir, "test.key")
	if err := os.WriteFile(privPath, privPEM, 0600); err != nil {
		t.Fatalf("failed to write PEM: %v", err)
	}
	privKey, err := LoadPrivateKeyFromFile(privPath)
	if err != nil {
		t.Fatalf("LoadPrivateKeyFromFile failed: %v", err)
	}

	// Verify base64 public key
	pubKey, err := LoadPublicKeyFromBase64(pubB64)
	if err != nil {
		t.Fatalf("LoadPublicKeyFromBase64 failed: %v", err)
	}

	// Verify the public keys match
	if !bytes.Equal(privKey.PublicKey().Bytes(), pubKey.Bytes()) {
		t.Fatal("public key mismatch between generated private key and exported public key")
	}
}

func TestLoadPublicKeyFromBase64_Invalid(t *testing.T) {
	tests := []struct {
		name string
		b64  string
	}{
		{"not base64", "not-valid-base64!!!"},
		{"wrong length", base64.StdEncoding.EncodeToString(make([]byte, 32))},
		{"wrong prefix", base64.StdEncoding.EncodeToString(append([]byte{0x03}, make([]byte, 64)...))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadPublicKeyFromBase64(tt.b64)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestLoadPrivateKeyFromPEM_Invalid(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		content string
	}{
		{"garbage", "this is not a PEM file"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, tt.name+".pem")
			if err := os.WriteFile(path, []byte(tt.content), 0600); err != nil {
				t.Fatalf("failed to write test file: %v", err)
			}
			_, err := LoadPrivateKeyFromFile(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func generateTestKeyPair(t *testing.T) (*ecdh.PublicKey, *ecdh.PrivateKey) {
	t.Helper()
	privKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	return privKey.PublicKey(), privKey
}

func createTestFile(t *testing.T, dir string, size int) string {
	t.Helper()
	path := filepath.Join(dir, "testfile.bin")
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("failed to generate random data: %v", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
	return path
}

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open file: %v", err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("failed to hash file: %v", err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	pubKey, privKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	// 10 MiB file = 3 chunks (4+4+2)
	inputPath := createTestFile(t, tmpDir, 10*1024*1024)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	originalHash := fileSHA256(t, inputPath)

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	// Encrypted file must be larger than original
	encStat, _ := os.Stat(encPath)
	origStat, _ := os.Stat(inputPath)
	if encStat.Size() <= origStat.Size() {
		t.Fatal("encrypted file should be larger than original")
	}

	if err := DecryptFile(encPath, decPath, privKey); err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	decHash := fileSHA256(t, decPath)
	if originalHash != decHash {
		t.Fatal("decrypted file does not match original")
	}
}

func TestEncryptDecryptSmallFile(t *testing.T) {
	pubKey, privKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	inputPath := createTestFile(t, tmpDir, 100)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	originalHash := fileSHA256(t, inputPath)

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}
	if err := DecryptFile(encPath, decPath, privKey); err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	decHash := fileSHA256(t, decPath)
	if originalHash != decHash {
		t.Fatal("decrypted file does not match original")
	}
}

func TestEncryptDecryptExactChunkBoundary(t *testing.T) {
	pubKey, privKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	// Exactly 4 MiB = one full chunk
	inputPath := createTestFile(t, tmpDir, 4*1024*1024)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	originalHash := fileSHA256(t, inputPath)

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}
	if err := DecryptFile(encPath, decPath, privKey); err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	decHash := fileSHA256(t, decPath)
	if originalHash != decHash {
		t.Fatal("decrypted file does not match original")
	}
}

func TestEncryptDecryptEmptyFile(t *testing.T) {
	pubKey, privKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	inputPath := createTestFile(t, tmpDir, 0)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	// Encrypted empty file should be just the header
	encStat, _ := os.Stat(encPath)
	if encStat.Size() != int64(eciesHeaderSize) {
		t.Fatalf("encrypted empty file should be %d bytes (header only), got %d", eciesHeaderSize, encStat.Size())
	}

	if err := DecryptFile(encPath, decPath, privKey); err != nil {
		t.Fatalf("DecryptFile failed: %v", err)
	}

	decStat, _ := os.Stat(decPath)
	if decStat.Size() != 0 {
		t.Fatalf("decrypted file should be empty, got %d bytes", decStat.Size())
	}
}

func TestDecryptWithWrongKey(t *testing.T) {
	pubKey, _ := generateTestKeyPair(t)
	_, wrongPrivKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	inputPath := createTestFile(t, tmpDir, 1024)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	err := DecryptFile(encPath, decPath, wrongPrivKey)
	if err == nil {
		t.Fatal("expected decryption error with wrong key, got nil")
	}

	// Partial output should be cleaned up
	if _, statErr := os.Stat(decPath); !os.IsNotExist(statErr) {
		t.Fatal("partial output file should have been removed on error")
	}
}

func TestDecryptCorruptedChunk(t *testing.T) {
	pubKey, privKey := generateTestKeyPair(t)
	tmpDir := t.TempDir()

	inputPath := createTestFile(t, tmpDir, 1024)
	encPath := filepath.Join(tmpDir, "encrypted.enc")
	decPath := filepath.Join(tmpDir, "decrypted.bin")

	if err := EncryptFile(inputPath, encPath, pubKey); err != nil {
		t.Fatalf("EncryptFile failed: %v", err)
	}

	// Corrupt a byte in the ciphertext area (after header + chunk header)
	encData, _ := os.ReadFile(encPath)
	corruptOffset := eciesHeaderSize + 16 + 10 // header + chunk header + 10 bytes into ciphertext
	if corruptOffset < len(encData) {
		encData[corruptOffset] ^= 0xff
		os.WriteFile(encPath, encData, 0644)
	}

	err := DecryptFile(encPath, decPath, privKey)
	if err == nil {
		t.Fatal("expected decryption error with corrupted data, got nil")
	}
}

func TestIsEncryptedFile(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name      string
		firstByte byte
		encrypted bool
		wantErr   bool
	}{
		{"encrypted", 0x02, true, false},
		{"gzip", 0x1f, false, false},
		{"unknown", 0xAA, false, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(tmpDir, tt.name+".bin")
			os.WriteFile(path, []byte{tt.firstByte, 0x00, 0x00}, 0644)

			encrypted, err := IsEncryptedFile(path)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if encrypted != tt.encrypted {
				t.Fatalf("expected encrypted=%v, got %v", tt.encrypted, encrypted)
			}
		})
	}
}
