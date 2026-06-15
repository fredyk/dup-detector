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

## Workflow (regla del usuario)
**SIEMPRE hacer `git push` a remoto tras commitear en este repo** (instrucción permanente de JFMV 2026-06-15). No preguntar.

## Binario / cache
- `go install .` → `~/go/bin/dup-detector`.
- Cache MD5: `~/.cache/dup-detector/` → symlink a `/media/fred/SHARED/dup-detector-cache` (NVMe, fuera de secure6).

---

## ✅ HECHO
- [x] Refactor streaming SQLite (RAM 11.4 GB → ~650 MB). 12 tests verdes.
- [x] Guard de soundness para `--min-size`/`--max-size` en tree-dedup.
- [x] **#1** Tree-dups verificados por CONTENIDO (MD5) en `-c` antes de ofrecerlos para borrar
  (`VerifyTreePairsByContent`; tests `content_test.go`). Resuelve el footgun de backups (mtime preservado).
  Nota: en modo mtime (sin `-c`) los tree-dups siguen siendo size+mtime — es la elección explícita del usuario.
- [x] **#2** Guard generalizado: `dirStoreIncomplete` compara nº de ficheros reales en disco vs los que el
  store conoce bajo cada dir. Cubre min-size, max-size, `--exclude`/`--include` y hardlinks de un golpe.
- [x] **#3** `temp_store(FILE)` — evita spike de RAM al crear el índice sobre 10M+ filas.
- [x] **#4** `seenInodes` aligerado a `map[inodo]struct{}` (~2 GB → ~200 MB).
- [x] **#7** Warning cuando `maxDirsPerBucket` descarta grupos de dirs (no silent caps).
- [x] **#8** `progressive.go` y el flag `--no-progressive` ELIMINADOS (estaban muertos).
- [x] **#9** `DetectDups` (muerto) eliminado. Las slice-versions (`MtimeDups`/`ChecksumDups`/
  `FindTreeDupsByHash`) se MANTIENEN a propósito: son el oracle de los cross-check tests.
- [x] **#10** `CleanStaleStores` barre `scan-<pid>.db` huérfanos de runs muertos al arrancar.
- [x] **#11** Tras `Finalize` se sube `SetMaxOpenConns(NumCPU)` → lecturas concurrentes (verify paralelo).

- [x] **#12 (CRÍTICO — hallado en run REAL con pprof)** `checksumGroup` lanzaba **1 goroutine por
  fichero** del grupo de tamaño. Un grupo con millones de ficheros del mismo tamaño → millones de
  goroutines × ~2KB stack = **~12 GB de RSS** (sawtooth de GC). pprof lo probó: **77.753 goroutines en
  `checksumGroup.func1`** sobre un repro de 80k ficheros. FIX: **pool de workers fijo** (N goroutines
  tirando de un canal) → goroutines O(workers). Tras el fix: 77.759 → 6 goroutines.
  **LECCIÓN**: el benchmark de RAM previo usó `--no-progressive` y se mató ANTES de la fase MD5, así
  que NO exponía este leak. Para validar de verdad hay que dejar llegar a la fase MD5 con un grupo de
  tamaño grande.

## ⏳ PENDIENTE / ACEPTADO (deuda residual, baja prioridad)
- [ ] **#5** `allGroups` + `dupIndex` + `TreeDupPair`s en RAM = O(dups). **ACEPTADO por ahora**: está
  acotado por el TAMAÑO DEL RESULTADO, no por el input (12.8M ficheros con pocos dups → resultado pequeño).
  Revisar solo si aparece un caso patológico (millones de grupos de duplicados). FIX futuro: streamear output.
- [ ] **#6** `accum`/`byKey` del tree-dedup = O(nº de directorios). **ACEPTADO**: nº de dirs << nº de ficheros.
  Documentado aquí por si un árbol con decenas de millones de dirs lo hiciera relevante.
