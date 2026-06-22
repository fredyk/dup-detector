# Diseño: dedup de solapamiento parcial de directorios ("bloques" en 2 columnas)

> Estado: DISEÑO (no implementado). Autor: Opus, 2026-06-22. Pendiente de confirmar
> decisiones marcadas con ❓ antes de codificar. Esto toca BORRADO de ficheros →
> data-loss-critical; la red de seguridad son `go test` + verificación a mano.

## 1. Problema

Cuando dos directorios **no son idénticos** pero comparten **muchos ficheros**, el
detector hoy ofrece cada fichero compartido como un **grupo file-level independiente**
(`actionFileGroup`), así que el usuario lo resuelve **fichero por fichero** en la cola
interactiva. Para carpetas de backup (decenas/cientos de ficheros comunes) es tedioso.

**Objetivo:** ofrecer un **bloque** que muestre hasta **30 ficheros compartidos en 2
columnas** (dir A | dir B) y permita deduplicar la columna entera de una vez.

## 2. Prior art

`rmlint --merge-directories` y `jdupes` solo tratan directorios **totalmente**
duplicados (= nuestro `actionTree` ya existente). El solapamiento **parcial** con
revisión en 2 columnas no existe en las herramientas comunes → es funcionalidad nueva.

## 3. Modelo de datos existente (reutilizable)

- `ScannedFile{ Path, RelPath, Size, ModTime, Inode, Source(0=A,1=B) }`.
- `DupGroup{ Size, Files []ScannedFile }` — ficheros con contenido idéntico.
- `TreeDupState.dupIndex map[string][]string` (tras #15: slice de miembros del grupo
  compartida por referencia) → permite saber, para un fichero, sus duplicados.
- `cleanup.go`: cola unificada `cleanupAction` (`actionTree` | `actionFileGroup`),
  ordenada por `waste()` desc, prompt numérico uniforme; `[a]uto` es modo persistente.
- Modo `twoDir` cuando se invoca `dup-detector A B`: A tiene `Source=0`, B `Source=1`,
  y `RelPath` es relativo a cada root → emparejar por `RelPath` da las filas en 2 columnas.

## 4. Concepto central: un tercer `actionKind`

`actionDirOverlap` junto a `actionTree`/`actionFileGroup`. Un bloque es:

```
dirOverlapAction {
    dirA, dirB string          // las dos carpetas comparadas
    rows []overlapRow          // ≤ 30 filas (pares de ficheros idénticos)
    sharedBytes int64          // Σ Size de las filas (UNA copia) → para ordenar
}
overlapRow { a, b ScannedFile } // a bajo dirA, b bajo dirB, mismo contenido
```

`waste()` del bloque = `sharedBytes` (al borrar una columna se recupera una copia de
cada fila). Se inserta en la MISMA cola `actions` ordenada por bytes desc → **resuelve tu
refinamiento #1**: la suma de los tamaños del bloque-30 lo coloca en su sitio correcto
del flujo por tamaños totales.

### Por qué bloques de ≤30 (no "una carpeta = una acción gigante")

Un par de dirs que comparte K ficheros se parte en `ceil(K/30)` bloques de ≤30, **cada
uno una unidad de cola con su propia suma de tamaños** (tu framing literal). Ventajas:
prompt siempre escaneable; `[A]/[B]` actúa exactamente sobre lo que se ve; el "biggest
win first" se mantiene a granularidad de bloque. ❓**Orden de troceado**: por tamaño desc
(coherente con el resto del tool, agrupa los ficheros grandes en los primeros bloques) —
alternativa: por `RelPath` (agrupa ficheros relacionados de la misma subcarpeta). Propuesta:
**tamaño desc**.

## 5. Detección

### Señal base (ya existe)
Cada `DupGroup` enlaza directorios que comparten ≥1 fichero. La generación de pares
candidatos (`newCandidatePairs`, ancestor-walk con cap `maxDirsPerGroup=50`) ya acota la
explosión combinatoria de ficheros ubicuos.

### Cómputo del solapamiento por par de dirs
Para un par candidato (A,B) que **no** sea tree-dup completo: contar ficheros bajo A con
duplicado bajo B. Es `allCoveredByIndex` pero **contando** en vez de todo-o-nada →
helper nuevo `countCoveredByIndex(filesA, dirB, dupIndex) (count, bytes, rows)`. Emparejar
preferentemente por `RelPath` (misma ruta relativa en ambos árboles); si no coincide,
por contenido (cualquier duplicado bajo B).

### Guard de RAM (lección #12/#15/#16)
Materializar `filesA`/`filesB` de un dir enorme fue el spike #16. Para overlap **acotar a
pares de dirs con `min(countUnder A, countUnder B) ≤ overlapMaxDirFiles`** (p.ej. 50_000)
y `shared ≥ minOverlap`. Dirs gigantes no son "carpeta vs carpeta" que el humano revise.
Documentar el cap (no silent: warning si se descarta, regla #7).

### Selección de nivel (el problema difícil ❓)
En modo single-dir, el ancestor-walk genera el par (A,B) a varios niveles (carpetas hoja
y sus ancestros). Sin cuidado saldrían **acciones de bloque anidadas** (la misma fila en
`/bk1/sub` vs `/bk2/sub` y en `/bk1` vs `/bk2`). Propuesta: elegir el **nivel maximal**:
el par (A,B) más alto cuyo solapamiento no esté ya cubierto por un par ancestro con
solapamiento ≥ (igual cobertura). Reutiliza la idea de dominación de `treeDominated`
pero con conteo parcial. **Decisión de fase**:

- **Fase 1 (modo `dup-detector A B`)**: el par es A-root vs B-root (o las subcarpetas
  top de cada uno). SIN problema de nivel (un único par top). Cubre el caso real de
  "comparar dos carpetas de backup". **Implementar primero.**
- **Fase 2 (modo single-dir)**: descubrir pares de carpetas por overlap + dominación de
  nivel. Más sutil; diseño aparte una vez la Fase 1 esté validada.

❓**¿Tu flujo es `dup-detector A B` (dos carpetas) o `dup-detector RAÍZ` (una raíz con
muchas subcarpetas de backup dentro)?** Define si basta Fase 1 o hace falta Fase 2.

### Absorción (no ofrecer dos veces)
Los ficheros de un bloque también aparecerían como `actionFileGroup`. Regla: un grupo
file-level que sea un **par cruzado de 2 miembros** (uno bajo dirA, otro bajo dirB) de un
bloque emitido se **retira de `allGroups`** (lo "posee" el bloque). Grupos no absorbidos
(≥3 copias, o dirs que no forman bloque) siguen como file-groups. La Fase 1 (twoDir)
absorbe los grupos 2-miembros A↔B; el caso ≥3 copias (mismo fichero en 3+ backups) NO es
2-columnas → queda como file-group (o futura extensión a N columnas).

## 6. Prompt del bloque (UX)

```
[i/N] 4.2 GB reclaimable (folder overlap — 30 shared files, block 1/3)
  A: /backups/2024-01            (412 files, 187 shared in this pair)
  B: /backups/2024-02            (405 files, 187 shared in this pair)
  shared (block 1/3, 30 files, 4.2 GB):
    #    size     A  (rel)                         B  (rel)
    1    1.1 GB   video/holiday.mp4                video/holiday.mp4
    2    340 MB   photos/2024/IMG_0001.CR2         photos/2024/IMG_0001.CR2
    ...
   30    2 MB     docs/notes.pdf                   docs/notes.pdf
  Delete: [A] all A-side (keep B) · [B] all B-side (keep A)
          [f] file-by-file (this block only) · [s]kip · [a]uto · [q]uit · [?]help
```

- `[A]` / `[B]`: borra la columna entera del bloque (30 ficheros de ese lado), conserva
  la otra. **Nunca** borra ambas copias de una fila → siempre sobrevive una. Tras borrar,
  `pruneEmptyDirs` si la carpeta queda vacía.
- `[f]` **resolver fichero por fichero (solo este bloque)** → **tu refinamiento #2**:
  explota ESTE bloque en prompts por-fila (estilo `actionFileGroup`, 2 copias, elige cuál
  borrar), y al terminar **vuelve al modo bloque** para la siguiente acción. NO es modo
  persistente (a diferencia de `[a]`). Implementación: bandera local del bloque, no toca
  `autoMode`.
- `[a]` auto: mantiene el comportamiento global persistente existente (keep lado A /
  cadencia menos frecuente, borra el resto) — aquí "borra columna B, conserva A" salvo
  reorientación por cadencia de backup (reutiliza `reorientPairsByBackupCadence`, que ya
  decide qué lado conservar).
- Orientación A/B: aplicar `cadenceRegex`/`cadenceRank` ya existente para poner el lado
  de **retención más larga** (menos frecuente) como el que se conserva en `[a]`.

## 7. Seguridad (data-loss)

- Invariante: una fila siempre conserva ≥1 copia (las dos columnas son dirs distintos;
  borrar una columna deja la otra). El guard "rechaza borrar todas las copias" del
  file-group se traduce aquí en "no puedes borrar A y B de la misma fila".
- Si una fila comparte 3+ copias (otra fuera del par), borrar una sigue dejando ≥2 → ok.
- Re-resolución contra `deleted` al cargar cada acción (igual que hoy): si un bloque
  posterior solapa ficheros ya borrados por una acción previa, se filtran las filas; si
  quedan <1 filas vivas → skip automático.
- `[f]` reusa exactamente los primitivos `applyIndices`/`removeFile` ya probados.

## 8. Flags nuevos

- `--overlap` / `--no-overlap` (default ON en `-c`; ❓¿on por defecto?). Activa la
  agrupación en bloques.
- `--min-overlap N` (default p.ej. 4): mínimo de ficheros compartidos para formar bloque.
- `--overlap-block-size` (default 30): filas por bloque (tu "hasta 30").
- `overlapMaxDirFiles` interno (guard RAM), warning al descartar.

## 9. Plan de implementación (Fase 1, twoDir)

1. `overlap.go`: `BuildDirOverlaps(groups, lookup, twoDir, dirA, dirB, cfg) []dirOverlapAction`
   + `countCoveredByIndex` helper. Empareja por `RelPath`, trocea en bloques ≤30 por
   tamaño desc, calcula `sharedBytes`. Devuelve también los paths absorbidos.
2. `cleanup.go`: añadir `actionDirOverlap` a `cleanupAction`, `waste()`, `items()`,
   render de 2 columnas, opciones `[A]/[B]/[f]`, y `applyOverlap`. Retirar de `allGroups`
   los grupos absorbidos antes de construir la cola.
3. `main.go run()`: tras detección, `BuildDirOverlaps(...)`, pasar bloques + groups
   filtrados a `InteractiveDelete`.
4. Tests (`overlap_test.go`): detección de bloques, troceado ≤30, `sharedBytes` correcto,
   absorción (no doble oferta), `[A]/[B]` conserva una columna, `[f]` no persiste,
   ordenación por bytes en la cola, guard RAM. Cross-check: la unión de lo borrable por
   bloques == lo borrable file-by-file (sin el bloque) sobre el mismo fixture.
5. `go test` verde + verificación manual en un fixture de 2 carpetas con overlap parcial.

## 11. ADENDA — N roots vía `--additional-root` + impresión de roots (2026-06-22)

Requisito nuevo: analizar **más de 2 raíces**, pero **sin** meter más de 2 positional
args (propenso a errores). Solución: kwarg **repetible `--additional-root PATH`**.

### Modelo de roots
- `roots []string` = positional args (1-2, retrocompat) **+** cada `--additional-root`.
- Normalizar a absolutos, deduplicar, y **rechazar/avisar roots anidados** (un root
  dentro de otro) — reutiliza la lógica que ya excluye el subdir anidado en twoDir.
- `Source int` pasa de {0,1} a **índice de root 0..N-1**. `ScannedFile.Source` ya es int.
- **Al arrancar, imprimir la lista de roots numerada** que se va a analizar, p.ej.:
  ```
  Analyzing 3 roots:
    [0] /backups/2024-01
    [1] /backups/2024-02
    [2] /mnt/ext/backup-old
  ```
  (requisito explícito del usuario).

### Semántica cross-root (sustituye al `twoDir`/`hasBothSides`)
- `hasBothSides` (solo report si abarca A **y** B) se generaliza a
  **`spansMultipleRoots`**: un `DupGroup` se reporta si sus ficheros tocan **≥2 roots
  distintos** (ignora duplicados internos de un único backup). ❓¿Default cross-root-only
  cuando hay ≥2 roots, con flag `--within-root` para incluir también dups internos?
  Propuesta: **cross-root-only por defecto con ≥2 roots** (es lo que quieres al cruzar
  backups); 1 root = comportamiento single-dir actual (todos los dups).
- `twoDir` desaparece como bool → `len(roots) >= 2` + el filtro `spansMultipleRoots`.

### Overlap con N roots = pairwise 2 columnas (encaja con tu "2 columnas")
El solapamiento se calcula **por cada PAR de roots** (Ri, Rj), i<j → C(N,2) pares
(N=3 → 3 pares; manejable). Cada par produce sus bloques ≤30 emparejados por `RelPath`,
etiquetados con qué dos roots compara, y todos entran en la **misma cola ordenada por
bytes**. La vista sigue siendo SIEMPRE 2 columnas (Ri | Rj) — N solo amplía el scope de
escaneo, no la anchura del prompt.

Esto **simplifica la detección**: con roots explícitos no hace falta la Fase 2 (descubrir
pares de carpetas en single-dir). El emparejado es root-vs-root por `RelPath`. La Fase 2
(auto-discovery dentro de un único root) queda como extensión futura independiente.

### Caso ≥3 copias del mismo fichero (en 3+ roots)
Un fichero en los 3 roots aparece en los 3 pares (0-1, 0-2, 1-2). Si el usuario borra la
copia del root 0 en el bloque del par (0,1), las filas correspondientes en (0,2) se
auto-filtran por `deleted` al cargar esa acción (re-resolución ya existente). Hay que
asegurar que **no se borra la última copia global** del fichero: el guard por-fila
"conserva ≥1 de las 2 columnas" + el mapa `deleted` global lo cubren, pero hay que
verificar el orden (un fichero en 3 roots: como mucho se borran 2 de 3 copias a través de
2 bloques distintos, queda 1). Añadir test específico de 3-roots.

## 10. Decisiones CERRADAS (JFMV 2026-06-22)
1. **Scope**: roots explícitos (positional 1-2 + `--additional-root`). Overlap pairwise
   por par de roots. NO se hace auto-discovery single-dir (Fase 2) en v1.
2. **Qué reportar con ≥2 roots**: **solo cross-root** (dups que abarcan ≥2 roots). Dups
   internos de un mismo root se ignoran por defecto; flag `--within-root` para incluirlos.
   Con 1 root → comportamiento single-dir actual (todos).
3. **Emparejado de filas**: ⭐ por la **MISMA identidad de duplicado que la detección**:
   size+mtime en modo normal, **MD5 con `-c`**. NO por `RelPath`. Es decir, una fila =
   dos ficheros del **mismo `DupGroup`**, uno bajo Ri y otro bajo Rj. `RelPath` se usa
   SOLO para mostrar las dos columnas (y se puede resaltar si difieren = fichero movido).
   Implicación: un grupo puede tener varias copias en un mismo root → la "columna A" de la
   fila son TODAS las copias del grupo bajo Ri, "columna B" todas bajo Rj (normalmente 1:1
   en backups). Borrar columna A elimina las copias de Ri del grupo, conserva las de Rj.
4. **Granularidad**: `[A]/[B]/[f]` (sin selección de filas sueltas en v1; lo granular
   se hace con `[f]` fichero-por-fichero, no persistente).
5. **Bloques ≤30** ordenados por Σ tamaños en la cola global (refinamiento #1). ⭐ Los
   ficheros compartidos de un par de roots se ordenan **por tamaño DESC** y se trocean en
   bloques de 30 → **el bloque 1 contiene los 30 ficheros MÁS GRANDES**, el bloque 2 los
   30 siguientes, etc. (refinamiento JFMV: "los 30 más grandes"). Así el primer bloque de
   cada par es siempre el de mayor Σ tamaños y encabeza la cola por bytes.
6. **`[f]`** explota solo ese bloque, no persiste (refinamiento #2).
7. **Arranque**: imprimir lista numerada de roots a analizar.

### Pendiente menor (defaults, no bloquean)
- `--min-overlap N` (default 4), `--overlap`/`--no-overlap` (default ON), `--overlap-block-size`
  (default 30), `overlapMaxDirFiles` guard RAM. Ajustables tras primer uso.
