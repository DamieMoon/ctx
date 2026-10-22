// Parallel-Kanten-Index + Default-Krümmung (GC1 §4.2), aus @sigma/edge-curve
// 3.1.0 übernommen (MIT, © Alexis Jacomy / sigma.js contributors): sigma 4 hat
// das Paket gestrichen und spreizt Parallelkanten selbst — aber nach
// (index - (count-1)/2) · factor, was bei drei Kanten auf einem Paar der
// mittleren die Krümmung 0 gibt (gerade Kante, deckungsgleich mit einer
// dream-Linie). GraphPage bildet den Index deshalb weiter vorzeichen-erhaltend
// und nullfrei auf die `curvature`-Attribute ab, und GraphView registriert
// den gekrümmten Pfad ohne sigmas Spreizung (sigma-styles.ts).
//
// Reine graphology-Logik, vitest-node-tauglich; schreibt parallelIndex /
// parallelMinIndex / parallelMaxIndex wie das Original.

import type { AbstractGraph } from 'graphology-types'

/** Krümmung einer Solo-Kante (Original: DEFAULT_EDGE_CURVATURE). */
export const DEFAULT_EDGE_CURVATURE = 0.25

export function indexParallelEdgesIndex(graph: AbstractGraph): void {
  const nodeIds: Record<string, string> = {}
  const directedIdOf: Record<string, string> = {}
  const undirectedIdOf: Record<string, string> = {}

  let incr = 0
  graph.forEachNode((node) => {
    nodeIds[node] = String(++incr)
  })
  graph.forEachEdge((edge, _attrs, source, target) => {
    const s = nodeIds[source]
    const t = nodeIds[target]
    const directedId = `${s}-${t}`
    directedIdOf[edge] = directedId
    undirectedIdOf[directedId] = [s, t].sort().join('-')
  })

  const directed: Record<string, string[]> = {}
  const undirected: Record<string, string[]> = {}
  graph.forEachEdge((edge) => {
    const d = directedIdOf[edge]
    const u = undirectedIdOf[d]
    ;(directed[d] ??= []).push(edge)
    ;(undirected[u] ??= []).push(edge)
  })

  for (const d in directed) {
    const edges = directed[d]
    const directedCount = edges.length
    const undirectedCount = undirected[undirectedIdOf[d]].length

    if (directedCount === 1 && undirectedCount === 1) {
      // Allein, in beide Richtungen.
      graph.setEdgeAttribute(edges[0], 'parallelIndex', null)
      graph.setEdgeAttribute(edges[0], 'parallelMaxIndex', null)
    } else if (directedCount === 1) {
      // Allein in dieser Richtung, aber mit Gegenkante(n).
      graph.setEdgeAttribute(edges[0], 'parallelIndex', 1)
      graph.setEdgeAttribute(edges[0], 'parallelMaxIndex', 1)
    } else if (directedCount === undirectedCount) {
      // Mehrere, alle gleichgerichtet: symmetrisch um 0.
      const max = (directedCount - 1) / 2
      for (let i = 0; i < directedCount; i++) {
        graph.setEdgeAttribute(edges[i], 'parallelIndex', -max + i)
        graph.setEdgeAttribute(edges[i], 'parallelMinIndex', -max)
        graph.setEdgeAttribute(edges[i], 'parallelMaxIndex', max)
      }
    } else {
      // Mehrere, in beide Richtungen.
      for (let i = 0; i < directedCount; i++) {
        graph.setEdgeAttribute(edges[i], 'parallelIndex', i + 1)
        graph.setEdgeAttribute(edges[i], 'parallelMaxIndex', directedCount)
      }
    }
  }
}
