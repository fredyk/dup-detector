package main

import "testing"

// Un patrón con barra que NO empieza por "/" casa con el FINAL de la ruta,
// como en rsync: --exclude=prvt/informe.csv tiene que excluir el informe que
// el propio run está escribiendo dentro del árbol, esté a la profundidad que esté.
func TestPatronConBarraCasaConElFinalDeLaRuta(t *testing.T) {
	casos := []struct {
		patron, ruta string
		excluye      bool
	}{
		{"prvt/informe.csv", "home/fred/prvt/informe.csv", true},
		{"prvt/informe.csv", "prvt/informe.csv", true},
		{"prvt/informe.csv", "home/fred/aprvt/informe.csv", false},
		{"prvt/informe.csv", "home/fred/prvt/informe.csv.bak", false},
		{"/prvt/informe.csv", "home/fred/prvt/informe.csv", false},
		{"/prvt/informe.csv", "prvt/informe.csv", true},
		{"fred/prvt", "home/fred/prvt", true},
		{"b/*.csv", "a/b/c.csv", true},
		{"a/**/c.csv", "x/a/b/c.csv", true},
		{".flexiblefs", "a/.flexiblefs/x", true},
	}
	for _, c := range casos {
		reglas := []FilterRule{{Pattern: c.patron, Exclude: true}}
		if got := ShouldExclude(c.ruta, reglas); got != c.excluye {
			t.Errorf("ShouldExclude(%q, %q) = %v, quiero %v", c.ruta, c.patron, got, c.excluye)
		}
	}
}
