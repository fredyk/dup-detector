//go:build unix

package main

import (
	"os"
	"syscall"
)

// inodeKey returns the (Dev, Ino) tuple that uniquely identifies a file on
// disk. Two paths with the same key are hardlinks to the same inode —
// deleting one doesn't reclaim space while another link exists.
// Returns ok=false on platforms where stat info isn't a *syscall.Stat_t.
func inodeKey(info os.FileInfo) ([2]uint64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return [2]uint64{}, false
	}
	return [2]uint64{uint64(st.Dev), uint64(st.Ino)}, true
}

// tieneVariosNombres dice si al inodo apuntan dos o más entradas de directorio.
//
// Es lo único que hay que recordar para no contar un fichero dos veces: uno con
// un solo nombre no puede volver a aparecer en el recorrido, así que guardarlo
// en el mapa de vistos no evita ningún duplicado y cuesta una entrada por cada
// fichero del árbol. Con el mapa creciendo O(ficheros), pprof sobre un scan de
// /tank daba el 96 % del heap vivo colgando de scanWalk → maps.(*table).rehash,
// y el RSS subía a escalones cada vez que el mapa doblaba su capacidad.
func tieneVariosNombres(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Sin datos del inodo no se puede afinar: se recuerda, que es lo seguro.
		return true
	}
	return st.Nlink > 1
}
