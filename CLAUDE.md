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
- **Instalación: usar `./install.sh`** (build CGo + arregla shadows de PATH). OJO: `GOBIN=/unsafe/gopath/bin`
  (política de storage de JFMV), que **no está en el PATH**; el binario que JFMV ejecuta es `/home/fred/go/bin/dup-detector`
  (lo invoca por ruta completa con `sudo`). `go install .` a secas dejaría el build nuevo "escondido" en
  `/unsafe/gopath/bin` mientras `~/go/bin/dup-detector` (viejo) lo eclipsa → `install.sh` symlinka el shadow
  existente a la canónica para que `sudo /home/fred/go/bin/dup-detector` ejecute siempre el build fresco.
- Cache MD5: `~/.cache/dup-detector/` → symlink a `/fastunsafe/dup-detector-cache` (NVMe rápido).

---

## ✅ HECHO
- [x] **#20 (feature JFMV — `--headless` + `--trash`, orquestable sin TTY)** Modo no interactivo para que un
  agente pueda deduplicar sin el prompt interactivo. **`--headless`**: aplica la política *keep-first* del modo
  `a`/auto (conserva 1 copia por grupo — el lado de cadencia menos frecuente cuando los paths difieren solo en
  la cadencia — y elimina el resto) sin leer stdin. `HeadlessDelete` (`headless.go`) reusa los helpers extraídos
  `buildCleanupActions` + `reresolveAction` (antes inline en `InteractiveDelete`), así ambos drivers comparten
  orden y semántica. **`--trash`** (también válido en modo interactivo): en vez de `os.Remove`, mueve cada
  duplicado a la papelera freedesktop **del filesystem que lo contiene** — `<mountpoint>/.Trash-$uid` para un
  volumen montado, `$XDG_DATA_HOME/Trash` para el home — con `.trashinfo` spec-compliant (Path relativo al
  mountpoint en topdir-trash, absoluto en home-trash; percent-encoding por segmento). Detección de papelera por
  `st_dev` (`mountPointOf` sube hasta que cambia el device) → el move es siempre rename same-fs (nunca copia
  cross-device). Todo el borrado pasa ahora por un único choke point **`disposeFile`** (dry-run / trash / unlink),
  del que heredan `removeFile`/`deleteTree`/`deleteOverlapSide` → `--dry-run` y `--trash` funcionan idénticos en
  todos los kinds. TDD: `headless_test.go` (real borra todo-menos-1; dry-run no toca disco pero reporta; tree
  dry-run) + `trash_test.go` (mueve + escribe info; colisión de basename conserva ambos; headless+trash mueve en
  vez de borrar). Verificado e2e con el binario (dry-run conserva N, trash deja keep-first + papelera en el
  mountpoint tmpfs). `-race` verde.
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
  `DUP_DETECTOR_PPROF=off`. `install.sh` hace el build con `CGO_ENABLED=1` (mattn necesita gcc).
  Uso: `go tool pprof http://127.0.0.1:8158/debug/pprof/profile?seconds=30` (CPU) / `…/heap` (RAM) /
  `curl …/goroutine?debug=2` (stacks).
  - **Puerto dinámico + descubrimiento por PID (2026-06-23)**: con varios runs concurrentes solo uno podía
    coger 8158 (los demás se quedaban sin pprof — pasó en real). Ahora `startPprof` intenta 8158 y si está
    ocupado cae a **`127.0.0.1:0` (puerto efímero del SO)**, saca la dirección real de `ln.Addr()`, y escribe
    un fichero de descubrimiento **`<cachedir>/pprof/<pid>.json`** `{pid,addr,cmd,started}`. **`dup-detector
    pprof-list`** (subcomando, `cobra.NoArgs`) lista los endpoints vivos (filtra por PID vivo vía `Signal(0)`,
    EPERM=vivo para runs root). Limpieza: cleanup diferido borra el fichero al salir limpio; `sweepStalePprof`
    barre los de PIDs muertos al arrancar (como `CleanStaleStores`). NO se usó unix `.sock` a propósito:
    `go tool pprof`/`curl` consumen HTTP sobre TCP, un socket rompería el tooling estándar. El fichero por PID
    (idea de JFMV) cubre el descubrimiento; el TCP efímero, el transporte.

## ⏳ PENDIENTE / ACEPTADO (deuda residual, baja prioridad)
- [x] **#13 (CPU — hallado con pprof en run REAL de 13h sobre `/tank/secure4`, 10.9M ficheros)** El cuello
  de CPU NO era el MD5 (solo ~3% `pread`/lectura; la cache sirve 8M hits, casi 0 recálculos). **El 72% del
  tiempo estaba en `removeSubPairsFast` vía `TreeDupState.AddGroups`** (+22% GC inducido por sus allocs;
  `mapaccess1_faststr`/`memeqbody`/`aeshashbody` dominaban el self). CAUSA: `AddGroups` se llama **en cada batch**
  de la fase MD5 (`main.go` `onBatch`), y `removeSubPairsFrom(newPairs, s.Confirmed)` **reconstruía y re-ordenaba
  `append(reference…, pairs…)` con TODO `s.Confirmed` acumulado en cada llamada** → O(batches × |Confirmed|) ≈
  **cuadrático**, copiando el slice entero cada vez (de ahí el 22% GC). FIX: índice dominador **persistente**
  `TreeDupState.partnerOf` (poblado vía `addConfirmed`/`AddConfirmed`, único escritor de `Confirmed`); `AddGroups`
  ahora hace **una pasada incremental** (pares nuevos ordenados por longitud asc, `treeDominated` contra el mapa
  persistente, registrar) → O(|batch|·depth). `removeSubPairsFrom` ELIMINADA; `removeSubPairsFast` refactorizada
  para reusar `treeDominated`. `main.go` siembra los `earlyTrees` con `AddConfirmed` (no append directo) para
  mantener el invariante partnerOf↔Confirmed. **Equivalencia probada**: mismo `Confirmed` que el código viejo
  (verificado con stash en el escenario multi-batch). Bench sintético (8000 pares / 400 batches): **2.48 s →
  13.2 ms (~188×), 1.09 GB → 5.4 MB allocados**. Tests nuevos: `incremental_test.go` (one-shot, multi-batch
  characterization, AddConfirmed-seeds-dominators). NOTA pre-existente preservada: con candidatos generados por
  ancestros + gating de `checkedPairs`, un par alto `(P,Q)` propuesto en un batch temprano y rechazado por
  `dupIndex` incompleto NO se re-verifica luego → orden-dependiente (documentado en el test). ⏳ Re-validar en run
  real a escala (el run de 13h usaba binario viejo).
- [x] **#15 (CPU→RAM — hallado con captura pprof minuto-a-minuto en run REAL sobre `/tank/secure4 backups/me`)**
  Tras el #13, el run NO petaba por CPU sino por **RAM en runaway exponencial: 4→9→22→34→47 GB RSS en 4 min**, y
  el OS lo mató (la máquina tiene 86 GB; en una normal habría fundido el equipo). Heap dump final concluyente:
  **97.6% del heap (23.8 GB) en `main.mergeDedupe` vía `AddGroups`**. CAUSA: `AddGroups` construía, por cada uno
  de los k ficheros de un grupo de duplicados, su lista privada de los otros k-1 → **O(k²) strings por grupo** en
  el map `dupIndex`, que NO se libera en todo el run. Un único grupo de backup con decenas de miles de ficheros
  idénticos (k grande) → ~10⁹ strings → decenas de GB. Mismo patrón k² que el #12 (goroutines), ahora en memoria.
  FIX (opencode/deepseek, **revisado y corregido por Opus**): **una slice de miembros compartida por grupo**,
  asignada por referencia a cada fichero → **O(k) por grupo**. `mergeDedupe` eliminada (sin uso). ⚠️ Corrección de
  Opus sobre el trabajo de opencode: la slice compartida incluye al propio fichero, así que `allCoveredByIndex`
  ahora **salta la entrada self** — sin eso, un par candidato anidado (un dir ancestro del otro, con counts
  iguales) casaría un fichero consigo mismo y confirmaría un tree-dup falso → **borrado erróneo**. Tests:
  `TestAllCoveredByIndexSkipsSelf` (lock del self-skip) + `TestDupIndexMemoryLinear` (2000 idénticos → ~k, no k²).
  Evidencia del run en `/fastunsafe/dup-detector-profiling/run-20260618-161153/`. ⏳ Re-validar a escala.
- [x] **#16 (RAM — secundario, NO mató el proceso)** Spike transitorio de `FileStore.FilesUnderDir` (vía CGo
  `_Cfunc_GoStringN`) que materializa TODOS los strings de ruta de un dir candidato grande durante la verificación
  de pares de árbol en `verifyPair`/`AddGroups` (pprof: pico de 2.4 GB RSS / 10.7 GB virtual). El count-first (#5)
  lo evita SOLO cuando los counts no cuadran; un candidato de alto nivel que SÍ cuadra aún carga millones de
  strings. FIX: nueva función `CoverageAndSize` en `filestore.go` que streamea la verificación de cobertura
  fila por fila via `StreamFilesUnderDir`, más campo `CoverageCheck` en `TreeDupState` para que `verifyPair`
  use la versión streaming cuando está disponible (store-backed). La función nunca construye un `[]ScannedFile`
  — procesa cada fila SQLite una a una, short-circuit en cuanto un fichero falla la cobertura.
  ⚠️ **REVIEW Opus 2026-06-22**: el primer corte de opencode tenía un BUG CRÍTICO de data-loss:
  `CoverageAndSize` **siempre devolvía `covered=true`** (el `return false` del closure solo cortaba la
  iteración, no registraba la no-cobertura) → verificación de tree-dup DESACTIVADA → habría confirmado
  árboles NO duplicados para borrado. Los tests no lo cazaban porque la ruta `CoverageCheck` solo se usa en
  el run real (no en tests, que caen al fallback `allCoveredByIndex`). FIX Opus: flag `covered` capturado +
  guard `any` (dir vacío → false). Test nuevo `TestCoverageAndSizeRejectsUncovered` (filestore_test.go) que
  ejerce la ruta store-backed y **falla con el código buggy** (verificado con stash).
- [x] **#17 (feature pedida por JFMV — solapamiento PARCIAL de directorios en 2 columnas + N roots)**
  Cuando dos roots no son idénticos pero comparten muchos ficheros, antes salían fichero-a-fichero en la cola
  interactiva. Ahora se agrupan en **bloques de hasta 30 ficheros compartidos (los 30 MÁS GRANDES primero)** en
  **2 columnas** (`actionDirOverlap` en `cleanup.go`). Diseño completo en `docs/dir-overlap-design.md`.
  Componentes: (1) **N roots** vía `--additional-root` (repetible; sin más de 2 positional), imprime la lista
  numerada de roots al arrancar, `Source` = índice de root, anidados auto-excluidos; `hasBothSides` →
  `spansMultipleSources` (≥2 sources distintos; el viejo fallaba para roots 2,3 sin el 0). Cross-root-only por
  defecto con ≥2 roots (`--within-root` para incluir internos). (2) **`overlap.go` `BuildOverlapBlocks`**:
  empareja por IDENTIDAD de duplicado (size+mtime, o MD5 con `-c`), NO por RelPath; solo grupos de exactamente
  2 roots; trocea por tamaño desc; `sharedBytes` para ordenar en la cola global. (3) **UI**: `[1]`/`[2]` borra
  una columna entera (guarda: nunca borra la última copia de un fichero), `[f]` resuelve ese bloque
  fichero-a-fichero SIN persistir, `[s]`/`[a]`/`[q]`; reorientación por cadencia de backup. Flags
  `--no-overlap`, `--min-overlap` (default 2), `--overlap-block-size` (default 30). El report a stdout sigue
  mostrando todos los grupos (sin absorción) → no desaparece nada. Tests: `overlap_test.go` (troceado
  30-largest, sharedBytes, umbral, exclusión 3-roots, **guarda de borrado de última copia**), `roots_test.go`.
  Verificado end-to-end (borrado de columna + `[f]`). ⏳ Futuro: output estructurado (json/csv) de bloques;
  auto-discovery de pares de carpetas en single-dir (Fase 2 del diseño); overlap N-way (>2 roots por bloque).
- [x] **#18 (CPU — hallado con pprof en run REAL, binario con todos los fixes)** Tras #15, la RAM quedó plana
  (~5 GB plateau, sin runaway; `mergeDedupe` desaparecido). El NUEVO cuello era CPU: **98% en
  `runtime.cgocall → store.CountUnderDir`**, llamado por `verifyPair` (guard count-first del #5) para CADA par
  candidato de dirs (`countUnder(dk.a)` + `countUnder(dk.b)`) → millones de `SELECT COUNT(*)` indexados vía CGo.
  CAUSA: `CountUnderDir(d)` es determinista tras `Finalize()` (store read-only), pero el mismo dir recurre en
  muchísimos pares y se recalculaba cada vez. FIX: **`memoizeDirCounter`** (`tree_dup.go`) — cache `map[string]int`
  concurrency-safe (verifyPair corre en workers paralelos) que envuelve `treeState.CountUnder` en `main.go`.
  Dirs << ficheros → mapa barato. Test `TestMemoizeDirCounter`. Heap a 5 GB es working set LINEAL legítimo:
  `_Cfunc_GoStringN` ~1.8 GB (strings de ruta de SQLite), `checkedPairs` ~765 MB, `dupIndex` ~372 MB.
- [x] **#19 (Fase 2 del overlap — auto-discovery single-root + regla no-borrar-dirs, JFMV)** El overlap parcial
  ahora también funciona con **UN solo root** (antes gated a ≥2). En vez del frágil heurístico de dominación de
  nivel (peligroso en código que borra), uso **"roots virtuales" = subdirs a profundidad fija bajo el root**
  (`virtualRootOf`, default depth 1 = hijos directos del root, flag `--overlap-depth`). Esto fija el nivel de
  columna sin ambigüedad → no hay emparejamientos raros. `BuildOverlapBlocks` refactorizada para tomar una
  `columnOf func(ScannedFile) string` (multi-root → `roots[Source]`; single-root → `virtualRootOf`), unificando
  ambos modos. **REGLA JFMV: un borrado de overlap 2-columnas SOLO borra ARCHIVOS, NUNCA subcarpetas** (un
  solapamiento parcial significa que las carpetas NO son idénticas → su estructura se respeta, incluso dirs que
  queden vacíos). `deleteOverlapSide` ya no llama a prune (eliminada `pruneEmptyDirsUp`). Tests: `TestVirtualRootOf`,
  `TestBuildOverlapBlocksSingleRoot`, `TestDeleteOverlapSideKeepsDirectories`. Verificado e2e (single-root
  `/tmp/sr2` → bloque A↔B, borrar columna deja dirs intactos). NOTA: dos subdirs hermanos IDÉNTICOS salen como
  tree-dup Y como bloque overlap (redundancia menor, no es bug — el `deleted`+re-resolución lo hacen seguro; el
  tree-dup sí borra dirs porque es su función). ⏳ Pendiente aún: overlap N-way (>2 columnas), output json/csv de bloques.
- [x] **#14 (feature pedida por JFMV)** TTL de
  re-verificación de la cache MD5. Flag **`--cache-max-age`**
  (duration, **default 14d**, **`0` = desactivado** = comportamiento actual: confiar para siempre). Semántica:
  en `cache.Hash`, un hit (`size+mtime+inode` iguales) solo se reusa si además `seen ≥ now − max-age`; si no,
  se re-hashea y se actualiza `seen`. La columna `seen` (`cache.go`) YA se escribe solo en `store()` (al
  (re)calcular), NUNCA en un hit → es exactamente la semántica correcta de "última verificación por contenido";
  **NO bumpear `seen` en hits** (si no, el TTL nunca dispara). Cambio mínimo: añadir `seen` al `SELECT`
  (`cache.go:177`) + condición de frescura en la rama de hit (`cache.go:218`). Sin migración (filas viejas ya
  tienen `seen`). VALOR: red de seguridad contra contenido que cambia SIN tocar size/mtime/inode (bit rot,
  mtime preservado por `rsync -a`/`touch -r`). Es estrictamente seguro (recalcular solo da un hash más fresco);
  el coste es releer disco ≤1 vez por fichero cada `max-age`. ⏳ Implementado 2026-06-18.

- [x] **#5 (re-diagnosticado con pprof — NO era el result-set)** El pico de RAM de la fase MD5 (heap 4.4GB,
  RSS ~10GB) era **`FileStore.FilesUnderDir` desde `AddGroups`**: materializaba TODOS los ficheros bajo cada
  dir candidato (strings de ruta) solo para comparar conteos; dirs de alto nivel = millones de ficheros →
  slices multi-GB, y el verify paralelo lo amplificó (#11). FIX: `CountUnderDir` (COUNT
  indexado por rango) + **count-first** en los dos paths de verify (`AddGroups` vía `DirCounter`, y
  `verifyTreePairMtimeStore`/`dirStoreIncomplete`). ⏳ Re-validación a escala pendiente (disco libre ahora: tank 1% usado).
- [x] **#6** `accum`/`byKey` del tree-dedup = O(nº de directorios). **ACEPTADO**: nº de dirs << nº de ficheros.
