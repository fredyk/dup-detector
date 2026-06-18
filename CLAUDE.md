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

## Excludes por defecto (built-in)
`defaultExcludes` en `main.go` lista nombres SIEMPRE ignorados, independientemente de flags.
Actualmente: **`.flexiblefs`** (dirs de metadatos de FlexibleFS — bookkeeping interno, nunca
duplicados reales del usuario). Se insertan como la PRIMERA regla de filtro, así que un
`--include .flexiblefs` explícito puede re-incluirlos (last-match-wins, semántica rsync).

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

- [x] **#1** Driver SQLite: `modernc.org/sqlite` (Go puro) → **`mattn/go-sqlite3` (C-SQLite, CGo)** para
  ops de BD más rápidas. DSN cambiado a sintaxis mattn (`?_busy_timeout=…&_journal_mode=…`). ⚠️ El build
  ahora **necesita CGo** (gcc + libc) — se pierde el binario puro-Go. Verificado: abre la cache existente
  (creada por modernc, en NTFS/SHARED) con WAL OK. Impacto real **modesto** (la BD no es el cuello de
  botella; domina el I/O de disco), pero gratis una vez hecho.

- [x] **pprof live (on-by-default, loopback)** `pprof.go` arranca `net/http/pprof` en
  **`127.0.0.1:8158`** (solo loopback, nunca expuesto a red) al inicio de `run()`. Permite perfilar un
  run de horas EN VIVO (imposible a posteriori). Override con `DUP_DETECTOR_PPROF=":6060"`, desactivar con
  `DUP_DETECTOR_PPROF=off`. Bind best-effort: si el puerto está ocupado (otro run concurrente) avisa y
  sigue sin perfilar. `install.sh` (nuevo) hace el build con `CGO_ENABLED=1` (mattn necesita gcc) y recuerda
  la URL. Uso: `go tool pprof http://127.0.0.1:8158/debug/pprof/profile?seconds=30` (CPU) / `…/heap` (RAM) /
  `curl …/goroutine?debug=2` (stacks).

## ⏳ PENDIENTE / ACEPTADO (deuda residual, baja prioridad)
- [ ] **#13 (CPU — hallado con pprof en run REAL de 13h sobre `/tank/secure4`, 10.9M ficheros)** El cuello
  de CPU NO es el MD5 (solo ~3% es `pread`/lectura; la cache sirve 8M hits, casi 0 recálculos). **El 72% del
  tiempo está en `removeSubPairsFast` vía `TreeDupState.AddGroups`** (+22% GC inducido por sus allocs;
  `mapaccess1_faststr`/`memeqbody`/`aeshashbody` dominan el self). CAUSA: `AddGroups` se llama **en cada batch**
  de la fase MD5 (`main.go` `onBatch`), y `removeSubPairsFrom(newPairs, s.Confirmed)` (`tree_dup.go:213/515`)
  **reconstruye y re-ordena `append(reference…, pairs…)` con TODO `s.Confirmed` acumulado en cada llamada** →
  O(batches × |Confirmed|) ≈ **cuadrático** en nº de pares confirmados, copiando el slice entero cada vez
  (de ahí el 22% GC). FIX propuesto: mantener el set dominador de `s.Confirmed` de forma **incremental** (no
  recomputar desde cero por batch) — p.ej. índice persistente `partnerOf` en el state, o filtrar solo los
  `newPairs` contra una estructura ya construida una vez. ⏳ Sin implementar (run en curso usa binario viejo).
- [ ] **#14 (feature pedida por JFMV) TTL de re-verificación de la cache MD5** Flag **`--cache-max-age`**
  (duration, **default 14d**, **`0` = desactivado** = comportamiento actual: confiar para siempre). Semántica:
  en `cache.Hash`, un hit (`size+mtime+inode` iguales) solo se reusa si además `seen ≥ now − max-age`; si no,
  se re-hashea y se actualiza `seen`. La columna `seen` (`cache.go`) YA se escribe solo en `store()` (al
  (re)calcular), NUNCA en un hit → es exactamente la semántica correcta de "última verificación por contenido";
  **NO bumpear `seen` en hits** (si no, el TTL nunca dispara). Cambio mínimo: añadir `seen` al `SELECT`
  (`cache.go:177`) + condición de frescura en la rama de hit (`cache.go:218`). Sin migración (filas viejas ya
  tienen `seen`). VALOR: red de seguridad contra contenido que cambia SIN tocar size/mtime/inode (bit rot,
  mtime preservado por `rsync -a`/`touch -r`). Es estrictamente seguro (recalcular solo da un hash más fresco);
  el coste es releer disco ≤1 vez por fichero cada `max-age`. NOTA medida (18/06): con 14d hoy NO cambia nada
  (entrada más vieja de secure4 = 8 días); con 72h se re-hashearía ~50% (4.06M de 8.08M). ⏳ Sin implementar.

- [x] **#5 (re-diagnosticado con pprof — NO era el result-set)** El pico de RAM de la fase MD5 (heap 4.4GB,
  RSS ~10GB) era **`FileStore.FilesUnderDir` desde `AddGroups`**: materializaba TODOS los ficheros bajo cada
  dir candidato (strings de ruta) solo para comparar conteos; dirs de alto nivel = millones de ficheros →
  slices multi-GB, y el verify paralelo retenía varios (mi #11 lo amplificó). FIX: `CountUnderDir` (COUNT
  indexado por rango) + **count-first** en los dos paths de verify (`AddGroups` vía `DirCounter`, y
  `verifyTreePairMtimeStore`/`dirStoreIncomplete`). ⏳ Re-validación a escala pendiente (disco ocupado con moves).
- [ ] **#6** `accum`/`byKey` del tree-dedup = O(nº de directorios). **ACEPTADO**: nº de dirs << nº de ficheros.
  Documentado aquí por si un árbol con decenas de millones de dirs lo hiciera relevante.
