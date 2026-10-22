// parallel-edges.ts — die vier Fälle von indexParallelEdgesIndex, wie sie
// @sigma/edge-curve 3.1.0 schrieb (GC1 §4.2 hängt an genau diesen Werten).
import { expect, test } from 'vitest'
import { MultiDirectedGraph } from 'graphology'
import { DEFAULT_EDGE_CURVATURE, indexParallelEdgesIndex } from './parallel-edges'

function graphWith(edges: [string, string, string][]) {
  const g = new MultiDirectedGraph()
  for (const n of ['a', 'b', 'c']) g.addNode(n)
  for (const [key, s, t] of edges) g.addDirectedEdgeWithKey(key, s, t)
  indexParallelEdgesIndex(g)
  const at = (e: string) => [g.getEdgeAttribute(e, 'parallelIndex'), g.getEdgeAttribute(e, 'parallelMaxIndex')]
  return at
}

test('Solo-Kante: Index und Max null', () => {
  const at = graphWith([['e1', 'a', 'b']])
  expect(at('e1')).toEqual([null, null])
})

test('allein in dieser Richtung, mit Gegenkante: Index 1, Max 1', () => {
  const at = graphWith([
    ['ab', 'a', 'b'],
    ['ba', 'b', 'a'],
  ])
  expect(at('ab')).toEqual([1, 1])
  expect(at('ba')).toEqual([1, 1])
})

test('drei gleichgerichtete: symmetrisch um 0', () => {
  const at = graphWith([
    ['e1', 'a', 'b'],
    ['e2', 'a', 'b'],
    ['e3', 'a', 'b'],
  ])
  expect([at('e1'), at('e2'), at('e3')]).toEqual([
    [-1, 1],
    [0, 1],
    [1, 1],
  ])
})

test('mehrere in beide Richtungen: 1-basiert je Richtung', () => {
  const at = graphWith([
    ['ab1', 'a', 'b'],
    ['ab2', 'a', 'b'],
    ['ba1', 'b', 'a'],
  ])
  expect([at('ab1'), at('ab2'), at('ba1')]).toEqual([
    [1, 2],
    [2, 2],
    [1, 1],
  ])
})

test('Default-Krümmung wie edge-curve', () => {
  expect(DEFAULT_EDGE_CURVATURE).toBe(0.25)
})
