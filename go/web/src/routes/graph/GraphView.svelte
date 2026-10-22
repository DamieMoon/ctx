<script lang="ts">
  import { onMount } from 'svelte'
  import Sigma from 'sigma'
  import type { MouseCoords, WheelCoords } from 'sigma/types'
  import type { MultiDirectedGraph } from 'graphology'
  import { edgeVisible, nodeVisible, type GraphFilters } from '../../lib/graph/filters'
  import { remainingDegree, type EdgeAttrs, type NodeAttrs } from '../../lib/graph/graph-client'
  import type { GraphPalette } from '../../lib/graph/graph-theme'
  import { GRAPH_PRIMITIVES, graphStyles, type GraphLook } from '../../lib/graph/sigma-styles'

  let {
    graph,
    filters,
    palette,
    highlightedIds,
    topId = null,
    showLabels = true,
    onnodeclick,
    onnodedoubleclick,
  }: {
    graph: MultiDirectedGraph<NodeAttrs, EdgeAttrs>
    /** Instant client-side filtering via the reducers — no server roundtrip. */
    filters: GraphFilters
    /** Theme-aware colors (label/edge Sigma settings); from readGraphPalette. */
    palette: GraphPalette
    /** Nodes with an open floating window — all get the Sigma highlight (G5). */
    highlightedIds?: Set<string>
    /** Top (focused) window's node — rendered stronger (size bump). */
    topId?: string | null
    /** Display option (meta-row toggle): node labels on the canvas. OFF hides
     *  only the unlit labels — hover boxes and highlighted (open-window) nodes
     *  keep theirs (labelVisibility in sigma-styles.ts; renderLabels stays on,
     *  in sigma 4 it would switch off every label). */
    showLabels?: boolean
    /** Single click — opens a floating window for the node. */
    onnodeclick?: (id: string) => void
    /** Double click = expand (+1 hop), design 05-§2. */
    onnodedoubleclick?: (id: string) => void
  } = $props()

  let container: HTMLDivElement
  let renderer: Sigma<NodeAttrs, EdgeAttrs> | null = null
  // Palette + label toggle as live getters over the props: sigma 4 has no
  // setStyles, the style functions in sigma-styles.ts read these on every
  // evaluation — the effect below only has to refresh() (M5).
  const look: GraphLook = {
    get palette() {
      return palette
    },
    get showLabels() {
      return showLabels
    },
  }

  // Mount/kill with the component (design 04/05: sigma renders the
  // graphology instance directly; data changes go through graph mutations,
  // sigma picks them up via graphology events — no Svelte reactivity here).
  onMount(() => {
    const r = new Sigma<NodeAttrs, EdgeAttrs>(graph, container, {
      // GC1: structural edges (edge-attr `type` 'curvedArrow') render as curved
      // arrows — the path/head styles in sigma-styles.ts read that attribute.
      primitives: GRAPH_PRIMITIVES,
      styles: graphStyles<NodeAttrs, EdgeAttrs>(() => look),
      settings: {
        labelRenderedSizeThreshold: 6,
        labelDensity: 0.7,
        hideEdgesOnMove: true, // LOD (W4): edges drop during pan/zoom
        renderEdgeLabels: false,
        // sigma 4 defaults to "positions"; node/edge `size` attrs are pixel
        // sizes at the default zoom (v3 semantics) — keep them that way.
        itemSizesReference: 'screen',
      },
      // Filters hide, the top window's node grows, the degree badge ("· +N" =
      // visible incidences not loaded yet) decorates. Props are reactive
      // getters — the reducers always read the current filter object.
      nodeReducer: (node, data, attrs) => {
        if (!nodeVisible(attrs, filters)) return { visibility: 'hidden' }
        const badge = remainingDegree(attrs)
        const label = badge === null ? attrs.label : `${attrs.label} · +${badge}`
        // The top (focused) window's node reads stronger (design 07-§E); the
        // highlight itself is node STATE (effect below), never a graph attr.
        if (node === topId) return { label, size: data.size * 1.15 }
        return { label }
      },
      // sigma skips edges with hidden endpoints on its own — this only
      // applies the edge-level class/confidence gate.
      edgeReducer: (_edge, _data, attrs) => (edgeVisible(attrs, filters) ? {} : { visibility: 'hidden' }),
    })
    r.on('clickNode', ({ node }) => onnodeclick?.(node))
    r.on('doubleClickNode', ({ node, event }) => {
      event.preventSigmaDefault() // keep the default double-click zoom away
      onnodedoubleclick?.(node)
    })
    // AM-3 (U03-W5): Shift+Mausrad rotiert die Kamera. sigmas MouseCaptor
    // verdrahtet das Mausrad AUSSCHLIESSLICH auf Zoom und prüft dabei KEINE
    // Modifier. Das an den Renderer weitergereichte wheel-Event trägt aber die
    // originale DOM-WheelEvent (event.original) sowie preventSigmaDefault(), das
    // der MouseCaptor SYNCHRON nach dem Emit prüft und bei true den Default-Zoom
    // unterdrückt — der idiomatische Seam, um den Zoom zu übernehmen, statt einen
    // eigenen DOM-Wheel-Listener gegen sigmas eigenen zu setzen. Bei gehaltenem
    // Shift drehen wir camera.angle proportional zum (von sigma bereits
    // normalisierten) Delta und schlucken den Zoom; ohne Shift bleibt alles
    // unverändert (weiter Zoom). Touch-Zwei-Finger-Rotation ist bereits aktiv
    // (enableCameraRotation ist per Default true und wird hier nicht
    // überschrieben) — nicht angefasst. Nur Ego-GraphView: AM-3 meint „den
    // Graphen"; die OverviewMap-Cluster-Karte bleibt bewusst rotationsfrei.
    const ROTATE_STEP = 0.25 // rad je Wheel-Notch (sigma-Delta ~2.5 ⇒ ~0.6 rad)
    // Der Payload ist statisch MouseCoords; zur Laufzeit ist es beim Wheel eine
    // WheelCoords (trägt `.delta`) — daher der gezielte Cast.
    const rotateOnShiftWheel = ({ event }: { event: MouseCoords }) => {
      if (!event.original.shiftKey) return // ohne Shift: sigma zoomt (unverändert)
      event.preventSigmaDefault() // Default-Zoom unterdrücken
      const cam = r.getCamera()
      cam.setState({ angle: cam.getState().angle + (event as WheelCoords).delta * ROTATE_STEP })
    }
    r.on('wheelStage', rotateOnShiftWheel)
    r.on('wheelNode', rotateOnShiftWheel)
    // The highlight effect may have run before this mount (renderer null) —
    // seed the state once here so already-open windows light up.
    applyHighlight(r)
    renderer = r
    // Test-hook (design 03-§4/§6): the colors live in the WebGL canvas, not
    // the DOM — the Playwright gate reads them via getSetting/getNodeAttribute.
    // Dev/test only; never exposed in production builds.
    if ((import.meta.env.DEV || import.meta.env.VITE_E2E) && typeof window !== 'undefined') {
      ;(window as unknown as { __ctxGraph?: unknown }).__ctxGraph = { renderer: r, graph }
    }
    return () => {
      r.kill()
      renderer = null
    }
  })

  // Filter/selection changes re-run the reducers (filters arrives as a NEW
  // object from the panel, so reading the references is enough tracking).
  // Every open-window node is highlighted (backdrop + label, sigma-styles.ts),
  // the top one included; isHighlighted is sigma node STATE, mirrored into
  // getNodeDisplayData().highlighted.
  function applyHighlight(r: Sigma<NodeAttrs, EdgeAttrs>): void {
    const lit = new Set(highlightedIds ?? [])
    if (topId) lit.add(topId)
    graph.forEachNode((node) => {
      if (r.getNodeState(node).isHighlighted !== lit.has(node)) r.setNodeState(node, { isHighlighted: lit.has(node) })
    })
  }

  $effect(() => {
    void filters
    void highlightedIds
    void topId
    const r = renderer
    if (!r) return
    applyHighlight(r)
    r.refresh()
  })

  // G2: a theme switch hands down a NEW palette object (GraphPage reassigns its
  // $state on THEME_CHANGE_EVENT). Label/edge/hover colors read it through
  // `look`; the node/edge `color` attrs are re-baked on the shared instance by
  // GraphPage's recolorGraph; refresh() repaints both. The label toggle rides
  // the same getter. The first run before onMount is a safe no-op.
  $effect(() => {
    void palette
    void showLabels
    renderer?.refresh()
  })

  /** Re-center the camera (page calls this after a focus jump). */
  export function resetCamera(): void {
    renderer?.getCamera().reset({ duration: 300 })
  }

  /** Repaint after the page re-bakes node color attrs off-band (AM-2 hues
   *  arrive fire-and-forget after the initial seed merge, design 02a §A3 —
   *  recolor-on-arrival: GraphPage calls recolorGraph then this refresh). */
  export function refresh(): void {
    renderer?.refresh()
  }
</script>

<!-- data-e2e-mask: sigma/ForceAtlas2 pixel output is not seed-stable — the
     visual gate masks this region; semantics run via __ctxGraph (design 06 §4.3). -->
<div class="canvas" data-e2e-mask bind:this={container}></div>

<style>
  .canvas {
    width: 100%;
    height: 100%;
    /* S5: the 24rem floor was the pre-definite-height collapse guard; the
       canvas-mode height chain (GraphPage .area/.stage/.viewport) now carries a
       real height, so the canvas fills the region exactly with no floor fight. */
    min-height: 0;
    /* U02-W4: der Host bezieht seinen Hintergrund aus DERSELBEN Quelle, gegen die
       alle Kontrast-Gates (G1a–G1c, G4d) rechnen (--graph-bg) — nicht --surface-0.
       Beide sind heute wertgleich, aber eine künftige Divergenz ließe die Gates
       grün, während der echte Kontrast bricht. OverviewMap folgt --graph-bg bereits. */
    background: var(--graph-bg);
  }
  /* sigma creates its own canvases inside; nothing to style beyond the host. */
</style>
