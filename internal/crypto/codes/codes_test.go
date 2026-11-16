package codes

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

func key(seed byte) string {
	raw := make([]byte, keyLength)
	for i := range raw {
		raw[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func newProtector(t *testing.T) *Protector {
	t.Helper()
	p, err := NewProtector(key(1), key(100))
	if err != nil {
		t.Fatalf("no se pudo construir el protector: %v", err)
	}
	return p
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	p := newProtector(t)

	for _, code := range []string{"ABCD", "PRUEBA-0001", strings.Repeat("z", 64)} {
		enc, err := p.Encrypt(code)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", code, err)
		}
		if strings.Contains(enc, code) {
			t.Errorf("el criptograma contiene el código en claro")
		}

		got, err := p.Decrypt(enc)
		if err != nil || got != code {
			t.Errorf("Decrypt = %q, %v; esperaba %q", got, err, code)
		}
	}
}

func TestEncryptUsaUnNonceNuevoEnCadaCifrado(t *testing.T) {
	p := newProtector(t)

	a, errA := p.Encrypt("ABCD")
	b, errB := p.Encrypt("ABCD")

	if errA != nil || errB != nil {
		t.Fatalf("errores inesperados: %v, %v", errA, errB)
	}
	if a == b {
		t.Fatal("dos cifrados del mismo código no deben coincidir: el nonce es aleatorio")
	}
}

func TestDecryptRechazaUnCriptogramaInvalido(t *testing.T) {
	p := newProtector(t)
	enc, err := p.Encrypt("ABCD")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xff
	manipulado := base64.StdEncoding.EncodeToString(raw)
	otro, err := NewProtector(key(50), key(150))
	if err != nil {
		t.Fatal(err)
	}

	casos := map[string]struct {
		protector *Protector
		entrada   string
	}{
		"no es base64":           {p, "***"},
		"vacío":                  {p, ""},
		"truncado":               {p, base64.StdEncoding.EncodeToString([]byte("corto"))},
		"manipulado":             {p, manipulado},
		"cifrado con otra clave": {otro, enc},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			got, err := caso.protector.Decrypt(caso.entrada)
			if !errors.Is(err, ErrInvalidCiphertext) || got != "" {
				t.Fatalf("Decrypt = %q, %v; esperaba ErrInvalidCiphertext", got, err)
			}
		})
	}
}

func TestHMACEsDeterministicoDeLargoFijoYDependeDeLaClave(t *testing.T) {
	p := newProtector(t)
	otro, err := NewProtector(key(1), key(200))
	if err != nil {
		t.Fatal(err)
	}

	a, b := p.HMAC("ABCD"), p.HMAC("ABCD")

	if a != b {
		t.Error("el HMAC debe ser determinístico")
	}
	if len(a) != 64 {
		t.Errorf("largo del HMAC = %d, esperaba 64 (cabe en varchar(64))", len(a))
	}
	if p.HMAC("ABCE") == a {
		t.Error("códigos distintos deben dar HMAC distintos")
	}
	if otro.HMAC("ABCD") == a {
		t.Error("otra clave HMAC debe dar otro valor")
	}
	if p.HMAC("abcd") == a {
		t.Error("el HMAC distingue mayúsculas: el código se compara exacto")
	}
}

func TestNewProtectorRechazaClavesInvalidas(t *testing.T) {
	corta := base64.StdEncoding.EncodeToString(make([]byte, 31))

	casos := map[string][2]string{
		"AES no es base64":  {"***", key(100)},
		"HMAC no es base64": {key(1), "***"},
		"AES corta":         {corta, key(100)},
		"HMAC corta":        {key(1), corta},
		"AES vacía":         {"", key(100)},
		"claves iguales":    {key(1), key(1)},
	}
	for nombre, claves := range casos {
		t.Run(nombre, func(t *testing.T) {
			p, err := NewProtector(claves[0], claves[1])
			if !errors.Is(err, ErrInvalidKey) || p != nil {
				t.Fatalf("esperaba ErrInvalidKey, obtuvo %v", err)
			}
		})
	}
}

func TestElErrorDeClaveNoReflejaElValor(t *testing.T) {
	_, err := NewProtector("SECRETO-AES***", key(100))

	if err == nil || strings.Contains(err.Error(), "SECRETO") {
		t.Fatalf("el error refleja la clave: %v", err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestEncryptFallaSiNoHayAleatoriedad(t *testing.T) {
	p := newProtector(t)
	p.random = failingReader{}

	if _, err := p.Encrypt("ABCD"); err == nil {
		t.Fatal("esperaba un error si no se puede generar el nonce")
	}
}

func TestProtectorEsSeguroParaUsoConcurrente(t *testing.T) {
	p := newProtector(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enc, err := p.Encrypt("CONCURRENTE")
			if err != nil {
				t.Errorf("Encrypt: %v", err)
				return
			}
			if got, err := p.Decrypt(enc); err != nil || got != "CONCURRENTE" {
				t.Errorf("Decrypt = %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
}
