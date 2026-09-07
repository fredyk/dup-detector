package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// El mapa de inodos vistos existe para UNA cosa: no contar dos veces un fichero
// que aparece con varios nombres. Un fichero con nlink==1 no puede volver a
// aparecer, asi que guardarlo no sirve de nada y cuesta una entrada por cada
// fichero del arbol.
//
// A escala de /tank eso es lo que se come el heap: pprof en el scan vivo del
// 07/09/2026 daba el 96% del heap vivo colgando de
// main.scanWalk.func1 -> maps.(*table).rehash, y el rehash por duplicacion es el
// escalon que se veia en la serie del RSS. El arbol no se puede recorrer con una
// estructura O(ficheros) en RAM si el arbol tiene millones.
func TestScanWalkSoloRecuerdaInodosQuePuedenRepetirse(t *testing.T) {
	raiz := t.TempDir()

	const sueltos = 40
	for i := 0; i < sueltos; i++ {
		p := filepath.Join(raiz, fmt.Sprintf("suelto%02d.bin", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("contenido %d", i)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Un unico inodo con dos nombres: es el unico que hay que recordar.
	original := filepath.Join(raiz, "enlazado.bin")
	if err := os.WriteFile(original, []byte("me apuntan dos nombres"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(original, filepath.Join(raiz, "enlazado-otro-nombre.bin")); err != nil {
		t.Skipf("el sistema de ficheros de %s no admite hardlinks: %v", raiz, err)
	}

	seen := make(map[[2]uint64]struct{})
	var emitidos []ScannedFile
	cfg := &Config{}
	if err := scanWalk(raiz, cfg, nil, seen, func(f ScannedFile) error {
		emitidos = append(emitidos, f)
		return nil
	}); err != nil {
		t.Fatalf("scanWalk: %v", err)
	}

	// 1. La deduplicacion tiene que seguir intacta: los dos nombres del mismo
	//    inodo cuentan como un fichero.
	if len(emitidos) != sueltos+1 {
		t.Errorf("emitidos %d ficheros, esperaba %d (los %d sueltos y UNA vez el enlazado)",
			len(emitidos), sueltos+1, sueltos)
	}

	// 2. Y el mapa solo puede quedarse con el inodo que tiene mas de un nombre.
	//    Es el punto del arreglo: sin esto el mapa crece con el arbol entero.
	if len(seen) > 1 {
		t.Errorf("el mapa de inodos guarda %d entradas para un arbol de %d ficheros "+
			"con un solo inodo enlazado: es O(ficheros) en RAM", len(seen), sueltos+1)
	}
}
