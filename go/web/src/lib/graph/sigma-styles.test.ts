// Gate G4b (design 02-graph-darkmode §4.6) — Regressions-Garantie der theme-
// festen Hover-/Highlight-Optik, beobachtet über sigmas EIGENE Style-Auswertung
// (evaluateNodeStyle/evaluateEdgeStyle aus sigma/types — dieselbe Funktion, die
// der Renderer pro Node aufruft; keine Quelltext-Inspektion):
//   Rot-Referenz — sigmas DEFAULT_STYLES malen den Hover-Kasten '#ffffff' ohne
//                  Rahmen (der Defekt: Dark-Label darauf 2.59:1).
//   Grün        — graphStyles(palette) liefert palette.hoverBg als Fläche,
//                  palette.hoverStroke als 1px-Rahmen, #000-Schatten blur 8 und
//                  das Label in hoverLabel — für Hover UND Fenster-Highlight.
// Seit sigma 4 (Port 2026-10-22) statt des Canvas-2D-Recorders aus node-hover.
//
// Der Shim MUSS vor sigma stehen (WebGL-Globals, s. sigma-webgl-shim).
import './sigma-webgl-shim'
import { expect, test } from 'vitest'
import Graph from 'graphology'
import { DEFAULT_STYLES } from 'sigma'
import { evaluateEdgeStyle, evaluateNodeStyle, type BaseGraphState, type BaseNodeState } from 'sigma/types'
import type { GraphPalette } from './graph-theme'
import { graphStyles, type GraphLook } from './sigma-styles'

const PALETTE: GraphPalette = {
  labelColor: '#9aa0bb',
  edgeColor: '#5d5f80',
  edgeStrongColor: '#7d80a8',
  edgeStructuralColor: '#3d9478',
  nodeSat: 70,
  nodeLum: 68,
  hoverBg: '#1e1e28',
  hoverStroke: '#7d80a8',
  hoverLabel: '#d8dae5',
}

const ATTRS = { x: 10, y: 20, size: 5, label: 'Node A', color: '#abcabc' }

function nodeState(over: Partial<BaseNodeState> = {}): BaseNodeState {
  return { isHovered: false, isLabelHovered: false, isHidden: false, isHighlighted: false, isDragged: false, ...over }
}

const GRAPH_STATE: BaseGraphState = {
  isIdle: true,
  isPanning: false,
  isZooming: false,
  isDragging: false,
  hasHovered: false,
  hasHighlighted: false,
}

const graph = new Graph()

function evalNode(styles: unknown, state: BaseNodeState) {
  return evaluateNodeStyle(styles as Record<string, unknown>[], ATTRS, state, GRAPH_STATE, graph)
}

function styles(look: Partial<GraphLook> = {}) {
  return graphStyles(() => ({ palette: PALETTE, showLabels: true, ...look }))
}

test('G4b Rot-Referenz: sigmas DEFAULT_STYLES malen den Hover-Kasten weiß und ohne Rahmen', () => {
  const d = evalNode(DEFAULT_STYLES.nodes, nodeState({ isHovered: true }))
  expect(d.backdropVisibility).toBe('visible')
  expect(d.backdropColor).toBe('#ffffff')
  // Kein Rahmen → auf Dark verschwimmt der Kasten (kein Abhebungs-Träger).
  expect(d.backdropBorderWidth ?? 0).toBe(0)
})

test.each([
  ['Hover', { isHovered: true }],
  ['Fenster-Highlight', { isHighlighted: true }],
])('G4b Grün (%s): hoverBg-Fläche, 1px hoverStroke-Rahmen, #000-Schatten, Label in hoverLabel', (_name, over) => {
  const d = evalNode(styles().nodes, nodeState(over))
  expect(d.backdropVisibility).toBe('visible')
  expect(d.backdropColor).toBe('#1e1e28')
  expect(d.backdropBorderColor).toBe('#7d80a8')
  expect(d.backdropBorderWidth).toBe(1)
  expect(d.backdropShadowColor).toBe('#000')
  expect(d.backdropShadowBlur).toBe(8)
  expect(d.labelColor).toBe('#d8dae5')
  expect(d.labelVisibility).toBe('visible')
  expect(d.depth).toBe('topNodes')
})

test('ungeleuchtet: kein Kasten, Label in labelColor, Labels nach Dichte', () => {
  const d = evalNode(styles().nodes, nodeState())
  expect(d.backdropVisibility).toBe('hidden')
  expect(d.labelColor).toBe('#9aa0bb')
  expect(d.labelVisibility).toBe('auto')
  expect(d.depth).toBe('nodes')
})

test('Label-Schalter OFF blendet nur ungeleuchtete Labels aus', () => {
  const off = styles({ showLabels: false })
  expect(evalNode(off.nodes, nodeState()).labelVisibility).toBe('hidden')
  expect(evalNode(off.nodes, nodeState({ isHovered: true })).labelVisibility).toBe('visible')
  expect(evalNode(off.nodes, nodeState({ isHighlighted: true })).labelVisibility).toBe('visible')
})

test('Palette wird live gelesen: neuer Getter-Wert ohne neue Styles', () => {
  let palette = PALETTE
  const s = graphStyles(() => ({ palette, showLabels: true }))
  palette = { ...PALETTE, hoverBg: '#ffffff', labelColor: '#111111' }
  expect(evalNode(s.nodes, nodeState({ isHovered: true })).backdropColor).toBe('#ffffff')
  expect(evalNode(s.nodes, nodeState()).labelColor).toBe('#111111')
})

test('GC1: curvedArrow-Kanten werden gekrümmte Pfeile, alle anderen gerade Linien; Farbe aus dem Attribut', () => {
  const edgeState = {
    isHovered: false,
    isLabelHovered: false,
    isHidden: false,
    isHighlighted: false,
    parallelIndex: 0,
    parallelCount: 1,
  }
  const evalEdge = (attrs: Record<string, unknown>) =>
    evaluateEdgeStyle(styles().edges as Record<string, unknown>[], attrs, edgeState, GRAPH_STATE, graph)
  const structural = evalEdge({ type: 'curvedArrow', color: '#3d9478', size: 1.5 })
  expect(structural.path).toBe('curved')
  expect(structural.head).toBe('arrow')
  expect(structural.color).toBe('#3d9478')
  const dream = evalEdge({ color: '#7d80a8', size: 1.4 })
  expect(dream.path).toBe('straight')
  expect(dream.head).toBe('none')
  expect(evalEdge({ size: 1 }).color).toBe('#5d5f80') // Fallback palette.edgeColor
})
