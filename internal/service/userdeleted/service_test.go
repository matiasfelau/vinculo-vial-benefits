package userdeleted

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// fakeBurner simula la persistencia: quema los códigos del usuario y cuenta cuántos.
type fakeBurner struct {
	asignados map[uuid.UUID]int64
	err       error
	llamadas  []uuid.UUID
}

func (f *fakeBurner) BurnByUser(_ context.Context, userID uuid.UUID) (int64, error) {
	f.llamadas = append(f.llamadas, userID)
	if f.err != nil {
		return 0, f.err
	}
	quemados := f.asignados[userID]
	delete(f.asignados, userID)
	return quemados, nil
}

func TestExecuteQuemaLosCodigosDelUsuarioYDevuelveCuantos(t *testing.T) {
	baja, otro := uuid.New(), uuid.New()
	burner := &fakeBurner{asignados: map[uuid.UUID]int64{baja: 2, otro: 5}}

	quemados, err := NewBurnCodesService(burner).Execute(context.Background(), baja)

	if err != nil || quemados != 2 {
		t.Fatalf("Execute = %d, %v; esperaba 2", quemados, err)
	}
	if burner.asignados[otro] != 5 {
		t.Error("los códigos de otro usuario no debían tocarse")
	}
}

func TestExecuteEsIdempotente(t *testing.T) {
	baja := uuid.New()
	burner := &fakeBurner{asignados: map[uuid.UUID]int64{baja: 1}}
	svc := NewBurnCodesService(burner)

	primero, err1 := svc.Execute(context.Background(), baja)
	segundo, err2 := svc.Execute(context.Background(), baja)

	if err1 != nil || err2 != nil || primero != 1 || segundo != 0 {
		t.Fatalf("Execute = (%d, %v) y (%d, %v); esperaba 1 y luego 0 sin error", primero, err1, segundo, err2)
	}
}

func TestExecuteDeUnUsuarioSinCodigosNoEsUnError(t *testing.T) {
	quemados, err := NewBurnCodesService(&fakeBurner{}).Execute(context.Background(), uuid.New())

	if err != nil || quemados != 0 {
		t.Fatalf("Execute = %d, %v; esperaba 0 sin error", quemados, err)
	}
}

func TestExecutePropagaElErrorConLaCausa(t *testing.T) {
	causa := errors.New("base de datos caída")

	_, err := NewBurnCodesService(&fakeBurner{err: causa}).Execute(context.Background(), uuid.New())

	if !errors.Is(err, causa) {
		t.Fatalf("error = %v, esperaba que conservara la causa", err)
	}
}
