// Package codes protege los códigos de descuento en reposo: los cifra con AES-256-GCM
// para poder devolverlos al usuario y calcula su HMAC-SHA256 para deduplicar la carga y
// emparejar los canjes sin descifrar. Usa solo la biblioteca estándar.
package codes

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// keyLength es el largo en bytes de las dos claves.
const keyLength = 32

// ErrInvalidKey indica que una clave no es de 32 bytes en base64, o que las dos claves
// no son independientes. Nunca incluye el valor de la clave.
var ErrInvalidKey = errors.New("codes: clave inválida")

// ErrInvalidCiphertext indica que un código cifrado no se pudo descifrar: está
// truncado, corrupto o se cifró con otra clave.
var ErrInvalidCiphertext = errors.New("codes: código cifrado inválido")

// Protector cifra códigos y calcula su HMAC. Es seguro para uso concurrente.
type Protector struct {
	aead    cipher.AEAD
	hmacKey []byte
	random  io.Reader
}

// NewProtector construye el Protector con la clave AES y la clave HMAC, cada una de 32
// bytes en base64 e independientes entre sí.
func NewProtector(aesKeyBase64, hmacKeyBase64 string) (*Protector, error) {
	aesKey, err := decodeKey(aesKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: clave AES: %w", ErrInvalidKey, err)
	}
	hmacKey, err := decodeKey(hmacKeyBase64)
	if err != nil {
		return nil, fmt.Errorf("%w: clave HMAC: %w", ErrInvalidKey, err)
	}
	if hmac.Equal(aesKey, hmacKey) {
		return nil, fmt.Errorf("%w: la clave HMAC debe ser independiente de la clave AES", ErrInvalidKey)
	}

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("codes: no se pudo inicializar AES: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("codes: no se pudo inicializar GCM: %w", err)
	}
	return &Protector{aead: aead, hmacKey: hmacKey, random: rand.Reader}, nil
}

func decodeKey(encoded string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("no es base64")
	}
	if len(raw) != keyLength {
		return nil, fmt.Errorf("debe tener %d bytes", keyLength)
	}
	return raw, nil
}

// Encrypt cifra code con un nonce aleatorio nuevo y devuelve nonce y criptograma en
// base64. Dos cifrados del mismo código dan resultados distintos.
func (p *Protector) Encrypt(code string) (string, error) {
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := io.ReadFull(p.random, nonce); err != nil {
		return "", fmt.Errorf("codes: no se pudo generar el nonce: %w", err)
	}
	sealed := p.aead.Seal(nonce, nonce, []byte(code), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt descifra un valor producido por Encrypt.
func (p *Protector) Decrypt(encoded string) (string, error) {
	sealed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(sealed) < p.aead.NonceSize()+p.aead.Overhead() {
		return "", ErrInvalidCiphertext
	}
	nonce, ciphertext := sealed[:p.aead.NonceSize()], sealed[p.aead.NonceSize():]
	plain, err := p.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrInvalidCiphertext
	}
	return string(plain), nil
}

// HMAC devuelve el HMAC-SHA256 de code en hexadecimal (64 caracteres). Es
// determinístico: el mismo código siempre da el mismo valor.
func (p *Protector) HMAC(code string) string {
	mac := hmac.New(sha256.New, p.hmacKey)
	// hash.Hash.Write nunca devuelve error.
	_, _ = mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}
