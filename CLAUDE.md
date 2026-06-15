# dup-detector — estado del refactor + deuda de diseño (CLAUDE.md)

> Doc vivo para sobrevivir a compactaciones de sesión. Al retomar: `go test` (deben
> pasar 10), coger el primer `[ ]` por prioridad (🔴 antes), arreglar, `go test`,
> marcar `[x]`, commit. Actualiza este fichero a la vez que el código.

## Contexto
Detector de duplicados Go (ficheros + árboles de directorios idénticos) con cache MD5 SQLite.
**Refactor 2026-06-15**: la lista de ficheros pasó de RAM (`[]ScannedFile`) a un **store SQLite
on-disk en streaming**. Motivo: sobre `/mnt/mnt6` (12.8M ficheros) el binario llegaba a **11.4 GB
de RSS y earlyoom lo mataba** ("Terminated"). pprof: 99% del heap = strings de ruta
(`filepath.Join`) + `[]ScannedFile` (×3 copias: filesA, filesB, allFiles). Tras el refactor:
**~650 MB y plano** (benchmark a 1.6M ficheros). Detalle del diagnóstico: earlyoom `-m 8` + swap 0 + ARC sin capar.

## Arquitectura tras el refactor
- `scan.go`: `scanWalk` (recorrido único compartido) → `Scan` (slice, solo tests) y `ScanToStore` (streaming).
- `filestore.go`: `FileStore` SQLite (tabla `files`, índices `size`+`path`, inserts en batches de 50k,
  índices creados al final). Queries: `CollisionSizes/CollisionSizeCounts`, `FilesWithSize`,
  `IterAllByPath`, `FilesUnderDir` (range por prefijo). DSN: journal OFF, sync OFF, temp_store FILE.
- `store_detect.go`: `MtimeDupsStore`, `ChecksumDupsStore` (reutilizan `checksumGroup`/`groupByMtime`).
- `store_tree.go`: `FindTreeDupsByHashStore` (accum hash por dir, streaming) + `verifyTreePairMtimeStore`
  + `dirHasOutOfWindowFile` (guard min-size).
- `cleanup.go`/`tree_dup.go`: reciben `DirLookup` (closure store-backed) en vez de `[]ScannedFile`.
- `main.go run()`: todo vía store; sin `filesA`/`filesB`/`allFiles`.

## Tests (red de seguridad — `go test`)
10 tests: golden slice + cross-checks (store == slice) + guard min-size. TODOS verdes.

## Binario / cache
- `go install .` → `~/go/bin/dup-detector`.
- Cache MD5: `~/.cache/dup-detector/` → symlink a `/media/fred/SHARED/dup-detector-cache` (NVMe, fuera de secure6).

---

## ✅ HECHO
- [x] Refactor streaming SQLite (RAM 11.4 GB → ~650 MB).
- [x] Guard de soundness para `--min-size`/`--max-size` en tree-dedup (`dirHasOutOfWindowFile`).
- [x] #3 `temp_store(FILE)` — evita spike de RAM al crear el índice sobre 10M+ filas.
- [x] #4 `seenInodes` aligerado a `map[inodo]struct{}` (era `→string`, ~2 GB; ahora ~200 MB).

## ⏳ PENDIENTE (deuda de diseño, por prioridad)

### 🔴 Tier 1 — Correctness (footguns de pérdida de datos)
- [ ] **#1 Tree-dups confirmados por size+mtime, NO por contenido — incluso con `-c`.**
  Los "early trees" (`FindTreeDupsByHash*`, `Verified=false`) entran a `Confirmed` y se ofrecen para
  borrar sin checksum. Dos árboles mismo tamaño+mtime, contenido distinto → "idénticos" → pérdida.
  Peligroso en backups (rsync `-a` preserva mtime). FIX: en `-c`, verificar pares de árbol por MD5
  antes de ofrecerlos; o no ofrecer `Verified=false` para deleción (al menos warning rotundo).
- [ ] **#2 El guard min-size NO cubre `--exclude`/`--include`.** Ficheros excluidos también invisibles
  al store → misma unsoundness de tree-dedup. FIX: extender el chequeo o avisar.

### 🟠 Tier 2 — Memoria (el RESULTADO aún en RAM)
- [ ] **#5** `allGroups` + `dupIndex` (`map[ruta][]rutas`) + `TreeDupPair`s en RAM = O(dups).
  Patológico con millones de duplicados. FIX: streamear/paginar output; dupIndex al store.
- [ ] **#6** `accum`/`byKey` del tree-dedup = O(nº de directorios). Normalmente OK; documentar el límite.

### 🟡 Tier 3 — Completeness (caps silenciosos)
- [ ] **#7** `maxDirsPerBucket=2000`, `maxDirsPerGroup=50`, `depth=12` → pierde tree-dups SIN avisar.
  FIX: warning al topar cualquier cap (regla "no silent caps").

### ⚪ Tier 4 — Higiene / cabos sueltos
- [ ] **#8** `progressive.go` quedó MUERTO + flag `--no-progressive` es no-op (engañoso). FIX: borrar fichero+flag, o reconectar progressive al store.
- [ ] **#9** `DetectDups` + versiones slice (`MtimeDups`/`ChecksumDups`/`FindTreeDupsByHash`) solo las usan tests. Documentar como "test-only" o limpiar.
- [ ] **#10** Fuga de `scan-<pid>.db` huérfanos si el proceso muere antes de `Close()`. FIX: barrer restos viejos al arrancar.
- [ ] **#11** `FileStore` con `SetMaxOpenConns(1)` serializa el verify paralelo de `AddGroups`. Perf, no correctness. FIX: pool de conexiones read-only.
