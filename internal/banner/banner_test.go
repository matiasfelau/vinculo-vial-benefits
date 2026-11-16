package banner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/matiasfelau/vinculo-vial-benefits/internal/domain"
)

const maxBytes = 300 * 1024

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// webpBytes arma la cabecera de un WebP (RIFF....WEBPVP8 ): alcanza para la firma.
func webpBytes() []byte {
	return append([]byte("RIFF\x1a\x00\x00\x00WEBPVP8 "), make([]byte, 20)...)
}

func TestInspectDetectaElTipoPorLosBytes(t *testing.T) {
	casos := map[string]struct {
		data []byte
		tipo string
	}{
		"png":  {pngBytes(t), "image/png"},
		"jpeg": {jpegBytes(t), "image/jpeg"},
		"webp": {webpBytes(), "image/webp"},
	}
	for nombre, caso := range casos {
		t.Run(nombre, func(t *testing.T) {
			banner, err := Inspect(caso.data, maxBytes)

			if err != nil {
				t.Fatalf("error inesperado: %v", err)
			}
			if banner.ContentType != caso.tipo {
				t.Errorf("tipo = %q, esperaba %q", banner.ContentType, caso.tipo)
			}
			sum := sha256.Sum256(caso.data)
			if banner.ETag != hex.EncodeToString(sum[:]) || len(banner.ETag) != 64 {
				t.Errorf("ETag = %q, esperaba el SHA-256 en hexadecimal", banner.ETag)
			}
			if !bytes.Equal(banner.Data, caso.data) {
				t.Error("los bytes del banner no deben cambiar")
			}
		})
	}
}

func TestInspectRechazaLoQueNoEsPNGJPEGNiWebP(t *testing.T) {
	casos := map[string][]byte{
		"vacío":            {},
		"texto":            []byte("esto no es una imagen"),
		"gif":              []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"html":             []byte("<html><script>alert(1)</script></html>"),
		"svg":              []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"ejecutable":       []byte("MZ\x90\x00\x03\x00\x00\x00"),
		"png truncado a 3": pngBytes(t)[:3],
	}
	for nombre, data := range casos {
		t.Run(nombre, func(t *testing.T) {
			if _, err := Inspect(data, maxBytes); !errors.Is(err, domain.ErrInvalidBanner) {
				t.Fatalf("error = %v, esperaba ErrInvalidBanner", err)
			}
		})
	}
}

func TestInspectRespetaElTamañoMaximo(t *testing.T) {
	data := pngBytes(t)

	if _, err := Inspect(data, int64(len(data))); err != nil {
		t.Fatalf("justo en el tope debía aceptarse: %v", err)
	}
	if _, err := Inspect(data, int64(len(data))-1); !errors.Is(err, domain.ErrBannerTooLarge) {
		t.Fatalf("error = %v, esperaba ErrBannerTooLarge", err)
	}
}

func TestInspectElETagCambiaConElContenido(t *testing.T) {
	a, errA := Inspect(pngBytes(t), maxBytes)
	b, errB := Inspect(jpegBytes(t), maxBytes)

	if errA != nil || errB != nil {
		t.Fatalf("errores inesperados: %v, %v", errA, errB)
	}
	if a.ETag == b.ETag {
		t.Error("imágenes distintas deben tener ETag distinto")
	}
}
