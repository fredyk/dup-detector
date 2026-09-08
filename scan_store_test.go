package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// correr lanza el binario y devuelve stdout y stderr.
func correr(t *testing.T, bin string, args ...string) (string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v\nstderr:\n%s", args, err, stderr.String())
	}
	return stdout.String(), stderr.String()
}

// TestScanStoreReutilizaElInventarioEnVezDeVolverARecorrer: recorrer un arbol de
// millones de ficheros cuesta horas, y volver a lanzar la deteccion sobre el
// mismo arbol no puede pagarlas otra vez. Con --scan-store el inventario queda
// en disco y la segunda pasada arranca de el.
func TestScanStoreReutilizaElInventarioEnVezDeVolverARecorrer(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a/uno.txt"), "contenido duplicado")
	writeFile(t, filepath.Join(dir, "b/uno-copia.txt"), "contenido duplicado")

	inventario := filepath.Join(t.TempDir(), "inventario.db")
	primeraSalida, primeraTraza := correr(t, bin, "-c", "--no-interactive", "--scan-store", inventario, dir)
	if !strings.Contains(primeraTraza, "Scanning") {
		t.Fatalf("la primera pasada tiene que recorrer el arbol:\n%s", primeraTraza)
	}
	if _, err := os.Stat(inventario); err != nil {
		t.Fatalf("el inventario no quedo en disco: %v", err)
	}

	// Un par nuevo que la segunda pasada NO puede ver: es la prueba de que el
	// arbol no se volvio a recorrer.
	writeFile(t, filepath.Join(dir, "a/dos.txt"), "otro contenido duplicado")
	writeFile(t, filepath.Join(dir, "b/dos-copia.txt"), "otro contenido duplicado")

	segundaSalida, segundaTraza := correr(t, bin, "-c", "--no-interactive", "--scan-store", inventario, dir)
	if strings.Contains(segundaTraza, "Scanning") {
		t.Errorf("la segunda pasada volvio a recorrer el arbol en vez de reutilizar el inventario:\n%s", segundaTraza)
	}
	if segundaSalida != primeraSalida {
		t.Errorf("el informe cambio al reutilizar el inventario:\nprimera:\n%s\nsegunda:\n%s", primeraSalida, segundaSalida)
	}
}

// TestScanStoreReutilizadoNoDejaBorrar: un inventario viejo puede nombrar
// ficheros que ya no existen o ignorar los que llegaron despues. Sirve para
// informar, jamas para elegir que se borra.
func TestScanStoreReutilizadoNoDejaBorrar(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a/uno.txt"), "contenido duplicado")
	writeFile(t, filepath.Join(dir, "b/uno-copia.txt"), "contenido duplicado")

	inventario := filepath.Join(t.TempDir(), "inventario.db")
	correr(t, bin, "-c", "--no-interactive", "--scan-store", inventario, dir)

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-c", "--headless", "--scan-store", inventario, dir)
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Run(); err == nil {
		t.Fatal("reutilizar un inventario con --headless tiene que fallar: se borraria segun una foto vieja")
	}
	if !strings.Contains(stderr.String(), "--scan-store") {
		t.Errorf("el error tiene que nombrar --scan-store:\n%s", stderr.String())
	}
	for _, sub := range []string{"a/uno.txt", "b/uno-copia.txt"} {
		if _, err := os.Stat(filepath.Join(dir, sub)); err != nil {
			t.Errorf("no se pudo borrar nada: %s falta", sub)
		}
	}
}
