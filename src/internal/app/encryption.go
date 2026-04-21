package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

const (
	eciesVersion    byte   = 0x02
	eciesChunkSize  uint32 = 4 * 1024 * 1024 // 4 MiB plaintext per chunk
	eciesHeaderSize int    = 86              // 1 + 65 + 4 + 8 + 8
	eciesIVSize     int    = 12
	eciesHKDFInfo   string = "opsmill-upload-ecies"

	// defaultPublicKeyBase64 is the same static P-256 public key used in opsmill-upload.
	// Files encrypted with this key can be decrypted by the holder of the matching private key.
	defaultPublicKeyBase64 = "BGa4rFkHUGHIW4BscM7U5A/wnQlkN8CMUohu18sTC/qLEztz8Cm01YiyaRmrauCZK02gYJp51i+4GE9VAqzWF70="
)

// hkdfSHA256 derives a 32-byte AES-256 key from a shared secret using HKDF-SHA256.
// Parameters match opsmill-upload: salt = 32 zero bytes, info = "opsmill-upload-ecies".
func hkdfSHA256(sharedSecret []byte) ([]byte, error) {
	salt := make([]byte, 32)
	info := []byte(eciesHKDFInfo)

	// HKDF-Extract: PRK = HMAC-SHA256(salt, IKM)
	mac := hmac.New(sha256.New, salt)
	mac.Write(sharedSecret)
	prk := mac.Sum(nil)

	// HKDF-Expand: single iteration (32 bytes output = SHA-256 hash length)
	// T(1) = HMAC-SHA256(PRK, info || 0x01)
	mac = hmac.New(sha256.New, prk)
	mac.Write(info)
	mac.Write([]byte{0x01})
	return mac.Sum(nil), nil
}

// DefaultPublicKey returns the hardcoded opsmill-upload P-256 public key.
func DefaultPublicKey() (*ecdh.PublicKey, error) {
	return LoadPublicKeyFromBase64(defaultPublicKeyBase64)
}

// LoadPublicKeyFromBase64 parses a base64-encoded raw P-256 uncompressed public key (65 bytes).
func LoadPublicKeyFromBase64(b64 string) (*ecdh.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 public key: %w", err)
	}
	if len(raw) != 65 {
		return nil, fmt.Errorf("invalid public key length: expected 65 bytes, got %d", len(raw))
	}
	if raw[0] != 0x04 {
		return nil, fmt.Errorf("invalid public key format: expected uncompressed point (0x04 prefix), got 0x%02x", raw[0])
	}
	key, err := ecdh.P256().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid P-256 public key: %w", err)
	}
	return key, nil
}

// LoadPublicKeyFromFile reads a file containing a base64-encoded P-256 public key.
func LoadPublicKeyFromFile(path string) (*ecdh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read public key file: %w", err)
	}
	return LoadPublicKeyFromBase64(strings.TrimSpace(string(data)))
}

// LoadPrivateKeyFromFile reads a PEM-encoded PKCS8 EC private key and returns an ECDH private key.
func LoadPrivateKeyFromFile(path string) (*ecdh.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read private key file: %w", err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("failed to decode PEM block from %s", path)
	}

	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PKCS8 private key: %w", err)
	}

	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not an EC key (got %T)", key)
	}

	ecdhKey, err := ecdsaKey.ECDH()
	if err != nil {
		return nil, fmt.Errorf("failed to convert ECDSA key to ECDH: %w", err)
	}

	return ecdhKey, nil
}

// GenerateKeyPair generates a new P-256 ECDH keypair.
// Returns the PEM-encoded private key and the base64-encoded raw public key.
func GenerateKeyPair() (privateKeyPEM []byte, publicKeyBase64 string, err error) {
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("failed to generate key: %w", err)
	}

	// Marshal private key to PKCS8 PEM
	der, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal private key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: der,
	})

	// Export public key as base64 raw uncompressed point
	ecdhKey, err := privateKey.ECDH()
	if err != nil {
		return nil, "", fmt.Errorf("failed to convert to ECDH key: %w", err)
	}
	pubB64 := base64.StdEncoding.EncodeToString(ecdhKey.PublicKey().Bytes())

	return pemBytes, pubB64, nil
}

// EncryptFile encrypts a file using ECIES (P-256 ECDH + HKDF-SHA256 + AES-256-GCM).
// The output uses the V2 chunked format compatible with opsmill-upload.
func EncryptFile(inputPath, outputPath string, recipientPubKey *ecdh.PublicKey) (retErr error) {
	// Open input file and get size
	inFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open input file: %w", err)
	}
	defer inFile.Close()

	stat, err := inFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat input file: %w", err)
	}
	fileSize := uint64(stat.Size())

	// Generate ephemeral P-256 keypair
	ephemeralKey, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("failed to generate ephemeral key: %w", err)
	}

	// ECDH shared secret
	sharedSecret, err := ephemeralKey.ECDH(recipientPubKey)
	if err != nil {
		return fmt.Errorf("ECDH key exchange failed: %w", err)
	}

	// Derive AES-256 key
	aesKey, err := hkdfSHA256(sharedSecret)
	if err != nil {
		return fmt.Errorf("key derivation failed: %w", err)
	}

	// Create AES-GCM cipher
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to create GCM: %w", err)
	}

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		outFile.Close()
		if retErr != nil {
			os.Remove(outputPath)
		}
	}()

	// Calculate total chunks
	totalChunks := uint64(0)
	if fileSize > 0 {
		totalChunks = (fileSize + uint64(eciesChunkSize) - 1) / uint64(eciesChunkSize)
	}

	// Write header (86 bytes)
	header := make([]byte, eciesHeaderSize)
	header[0] = eciesVersion
	copy(header[1:66], ephemeralKey.PublicKey().Bytes())
	binary.BigEndian.PutUint32(header[66:70], eciesChunkSize)
	binary.BigEndian.PutUint64(header[70:78], fileSize)
	binary.BigEndian.PutUint64(header[78:86], totalChunks)

	if _, err := outFile.Write(header); err != nil {
		return fmt.Errorf("failed to write header: %w", err)
	}

	// Encrypt chunk by chunk
	plaintext := make([]byte, eciesChunkSize)
	iv := make([]byte, eciesIVSize)
	chunkHeader := make([]byte, 16) // 12B IV + 4B enc_len

	for i := uint64(0); i < totalChunks; i++ {
		// Read plaintext chunk
		n, err := io.ReadFull(inFile, plaintext)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return fmt.Errorf("failed to read chunk %d: %w", i, err)
		}

		// Generate random IV
		if _, err := rand.Read(iv); err != nil {
			return fmt.Errorf("failed to generate IV for chunk %d: %w", i, err)
		}

		// Encrypt
		ciphertext := gcm.Seal(nil, iv, plaintext[:n], nil)

		// Write chunk header: [12B IV] [4B enc_len BE]
		copy(chunkHeader[:12], iv)
		binary.BigEndian.PutUint32(chunkHeader[12:16], uint32(len(ciphertext)))

		if _, err := outFile.Write(chunkHeader); err != nil {
			return fmt.Errorf("failed to write chunk %d header: %w", i, err)
		}
		if _, err := outFile.Write(ciphertext); err != nil {
			return fmt.Errorf("failed to write chunk %d ciphertext: %w", i, err)
		}
	}

	return nil
}

// DecryptFile decrypts a file encrypted with ECIES V2 chunked format.
func DecryptFile(inputPath, outputPath string, privateKey *ecdh.PrivateKey) (retErr error) {
	inFile, err := os.Open(inputPath)
	if err != nil {
		return fmt.Errorf("failed to open encrypted file: %w", err)
	}
	defer inFile.Close()

	// Read header
	header := make([]byte, eciesHeaderSize)
	if _, err := io.ReadFull(inFile, header); err != nil {
		return fmt.Errorf("failed to read header: %w", err)
	}

	if header[0] != eciesVersion {
		return fmt.Errorf("unsupported encryption version: 0x%02x (expected 0x%02x)", header[0], eciesVersion)
	}

	// Parse header
	ephPubKeyBytes := header[1:66]
	// chunkSize at header[66:70] — read but not strictly needed for decryption
	fileSize := binary.BigEndian.Uint64(header[70:78])
	totalChunks := binary.BigEndian.Uint64(header[78:86])

	// Reconstruct ephemeral public key
	ephPubKey, err := ecdh.P256().NewPublicKey(ephPubKeyBytes)
	if err != nil {
		return fmt.Errorf("invalid ephemeral public key in header: %w", err)
	}

	// ECDH shared secret
	sharedSecret, err := privateKey.ECDH(ephPubKey)
	if err != nil {
		return fmt.Errorf("ECDH key exchange failed: %w", err)
	}

	// Derive AES-256 key
	aesKey, err := hkdfSHA256(sharedSecret)
	if err != nil {
		return fmt.Errorf("key derivation failed: %w", err)
	}

	// Create AES-GCM cipher
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("failed to create AES cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("failed to create GCM: %w", err)
	}

	// Create output file
	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer func() {
		outFile.Close()
		if retErr != nil {
			os.Remove(outputPath)
		}
	}()

	// Decrypt chunk by chunk
	chunkHeader := make([]byte, 16)
	var decryptedSize uint64

	for i := uint64(0); i < totalChunks; i++ {
		// Read chunk header
		if _, err := io.ReadFull(inFile, chunkHeader); err != nil {
			return fmt.Errorf("failed to read chunk %d header: %w", i, err)
		}

		iv := chunkHeader[:12]
		encLen := binary.BigEndian.Uint32(chunkHeader[12:16])

		// Read ciphertext
		ciphertext := make([]byte, encLen)
		if _, err := io.ReadFull(inFile, ciphertext); err != nil {
			return fmt.Errorf("failed to read chunk %d ciphertext: %w", i, err)
		}

		// Decrypt
		plaintext, err := gcm.Open(nil, iv, ciphertext, nil)
		if err != nil {
			return fmt.Errorf("decryption failed at chunk %d/%d: %w (wrong key or corrupted data)", i, totalChunks, err)
		}

		if _, err := outFile.Write(plaintext); err != nil {
			return fmt.Errorf("failed to write decrypted chunk %d: %w", i, err)
		}
		decryptedSize += uint64(len(plaintext))
	}

	if decryptedSize != fileSize {
		logrus.Warnf("Size mismatch: expected %d bytes, got %d bytes", fileSize, decryptedSize)
	}

	return nil
}

// IsEncryptedFile checks if a file is in ECIES encrypted format by reading its first byte.
// Returns true for encrypted files (0x02), false for gzip files (0x1f).
func IsEncryptedFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("failed to open file: %w", err)
	}
	defer f.Close()

	var firstByte [1]byte
	if _, err := io.ReadFull(f, firstByte[:]); err != nil {
		return false, fmt.Errorf("failed to read file header: %w", err)
	}

	switch firstByte[0] {
	case eciesVersion:
		return true, nil
	case 0x1f: // gzip magic byte
		return false, nil
	default:
		return false, fmt.Errorf("unrecognized file format: first byte 0x%02x (expected 0x02 for encrypted or 0x1f for gzip)", firstByte[0])
	}
}
