package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// reglaPorDefecto reproduce lo que run() mete SIEMPRE en cfg.Rules
// (defaultExcludes), que es lo que deja armada la guarda de completitud en
// cualquier ejecucion, lleve o no filtros el usuario.
func reglaPorDefecto() []FilterRule {
	return []FilterRule{{Pattern: ".flexiblefs", Exclude: true}}
}

// almacenar escanea los subdirectorios indicados de root en un store nuevo.
func almacenar(t *testing.T, root string, cfg *Config, subdirs ...string) *FileStore {
	t.Helper()
	fs, err := NewFileStore(filepath.Join(t.TempDir(), "scan.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i, d := range subdirs {
		if err := ScanToStore(fs, filepath.Join(root, d), cfg, nil, nil, i, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.Finalize(); err != nil {
		t.Fatal(err)
	}
	return fs
}

// TestElComprobadorNoVuelveAlDiscoParaElMismoDirectorio: la respuesta a "el
// store tiene todos los ficheros de este directorio" es fija dentro de un run,
// asi que preguntarla dos veces no puede costar dos recorridos de disco.
func TestElComprobadorNoVuelveAlDiscoParaElMismoDirectorio(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "A")
	writeFile(t, filepath.Join(dir, "uno.bin"), strings.Repeat("K", 512))
	writeFile(t, filepath.Join(dir, "dos.bin"), strings.Repeat("K", 512))

	cfg := &Config{Recursive: true, Rules: reglaPorDefecto()}
	fs := almacenar(t, root, cfg, "A")
	defer fs.Close()

	chk := newDirStoreChecker(fs)
	if chk.incompleteDir(dir) {
		t.Fatal("el store tiene los dos ficheros: el directorio esta completo")
	}

	// Si volviera al disco, el recorrido fallaria y la guarda diria "incompleto".
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if chk.incompleteDir(dir) {
		t.Fatal("volvio al disco a por una respuesta que ya tenia")
	}
}

// TestVerificarParesNoRecorreElDiscoUnaVezPorPar: k directorios identicos
// generan k(k-1)/2 pares, y cada par recorria en disco sus dos directorios. Con
// 3 directorios son 6 recorridos donde solo hacen falta 3; con 2000 (el tope de
// maxDirsPerBucket) son cerca de 4 millones.
func TestVerificarParesNoRecorreElDiscoUnaVezPorPar(t *testing.T) {
	root := t.TempDir()
	contenido := strings.Repeat("K", 512)
	var dirs []string
	for _, d := range []string{"A", "B", "C"} {
		ruta := filepath.Join(root, d)
		dirs = append(dirs, ruta)
		writeFile(t, filepath.Join(ruta, "uno.bin"), contenido)
		writeFile(t, filepath.Join(ruta, "dos.bin"), contenido)
		pinMtime(t, filepath.Join(ruta, "uno.bin"), filepath.Join(ruta, "dos.bin"))
	}

	cfg := &Config{Recursive: true, Rules: reglaPorDefecto()}
	fs := almacenar(t, root, cfg, "A", "B", "C")
	defer fs.Close()

	chk := newDirStoreChecker(fs)
	recorridos := map[string]int{}
	real := chk.walkCount
	chk.walkCount = func(dir string) (int, error) {
		recorridos[dir]++
		return real(dir)
	}

	pares := []TreeDupPair{
		{DirA: dirs[0], DirB: dirs[1], FileCount: 2},
		{DirA: dirs[0], DirB: dirs[2], FileCount: 2},
		{DirA: dirs[1], DirB: dirs[2], FileCount: 2},
	}
	verificados, err := verifyPairsMtimeStore(pares, fs, cfg, chk)
	if err != nil {
		t.Fatal(err)
	}
	if len(verificados) != 3 {
		t.Fatalf("los tres directorios son identicos: se esperaban 3 pares verificados, hubo %d", len(verificados))
	}
	for _, dir := range dirs {
		if recorridos[dir] != 1 {
			t.Fatalf("%s se recorrio %d veces en disco para 3 pares: el coste es O(pares), no O(directorios)",
				dir, recorridos[dir])
		}
	}
}

// TestNoInteractiveNoGastaLaPasadaDeArboles: el informe CSV es de grupos de
// FICHEROS —los pares de arbol no tienen fila en el—, asi que calcularlos es
// trabajo que se tira. Sobre /tank esa pasada se llevo 16 h sin escribir una
// sola linea del informe.
func TestNoInteractiveNoGastaLaPasadaDeArboles(t *testing.T) {
	bin := buildDupDetector(t)

	dir := t.TempDir()
	for _, d := range []string{"a", "b"} {
		writeFile(t, filepath.Join(dir, d, "uno.txt"), "contenido duplicado")
		writeFile(t, filepath.Join(dir, d, "dos.txt"), "otro contenido duplicado")
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-c", "--no-interactive", dir)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader("")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstderr:\n%s", err, stderr.String())
	}

	if strings.Contains(stderr.String(), "fast pass") {
		t.Errorf("--no-interactive lanzo la deteccion de arboles, que no sale en el CSV:\n%s", stderr.String())
	}

	rows := csvRows(t, stdout.String())
	if len(rows) != 5 {
		t.Fatalf("se esperaban cabecera + 4 ficheros duplicados, hubo %d filas:\n%s", len(rows), stdout.String())
	}
}
