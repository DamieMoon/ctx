// Theme-feste sigma-4-Optik für GraphView + OverviewMap (design 02-graph-darkmode
// §4.4 M4, U02-W3; Port sigma 3 → 4).
//
// sigma 3 malte den Hover-/Highlight-Kasten mit einem Canvas-2D-Drawer
// (`defaultDrawNodeHover`), dessen Default `fillStyle = '#FFF'` hartkodierte —
// auf Dark lag das Label darauf mit nur 2.59:1; node-hover.ts ersetzte ihn.
// sigma 4 rendert den Kasten als WebGL-„Backdrop" und Label-Farbe, Sichtbarkeit
// und Kantenform als deklarative Styles. Dieses Modul legt dieselbe Optik als
// Style-Regeln über sigmas DEFAULT_STYLES: Fläche palette.hoverBg, 1px-Rahmen
// palette.hoverStroke (der Abhebungs-Träger, Gate G4d), #000-Schatten blur 8,
// Label in palette.hoverLabel — für Hover UND Fenster-Highlight (beide liefen in
// v3 durch denselben Hover-Renderer, Inventur §4).
//
// Die Palette und der Label-Schalter kommen über einen Getter, weil sigma 4 kein
// setStyles kennt: die Style-Funktionen lesen bei jeder Auswertung den aktuellen
// Wert, der Aufrufer ruft beim Theme-Wechsel nur refresh() (M5).

import type { Attributes } from 'graphology-types'
import { DEFAULT_STYLES } from 'sigma'
import { extremityArrow, pathCurved, pathLine, pathLoop } from 'sigma/rendering'
import type { BaseNodeState, StylesDeclaration } from 'sigma/types'
import type { GraphPalette } from './graph-theme'

export interface GraphLook {
  palette: GraphPalette
  /** Display option: normale Node-Labels. OFF blendet NUR die ungeleuchteten
   *  aus — Hover- und Highlight-Labels bleiben (v3: drawHover lief unabhängig von
   *  renderLabels; v4 schaltet mit renderLabels ALLE Labels ab, deshalb lebt der
   *  Schalter hier als labelVisibility statt als Setting). */
  showLabels: boolean
}

/** Hover ODER Fenster-Highlight. Bewusst eine Funktion: ein Array-Prädikat
 *  (`whenState: ['isHovered', 'isHighlighted']`) ist in sigma 4 ein UND. */
export function isLit(state: BaseNodeState): boolean {
  return state.isHovered || state.isHighlighted
}

export const LABEL_FONT = 'ui-monospace, monospace'
export const LABEL_SIZE = 11

// Strukturelle Kanten wählen ihre Form über das Graph-Attribut `type`
// (graph-client mergeEgo: 'curvedArrow'); die Krümmung liest sigma 4 von selbst
// aus dem gleichnamigen Attribut `curvature` (GraphPage.settle). Der gekrümmte
// Pfad trägt KEINE sigma-Spreizung: die überschreibt `curvature` bei
// Parallelkanten mit (i - (n-1)/2)·f und gäbe bei drei Kanten auf einem Paar
// der mittleren 0 — siehe parallel-edges.ts.
const CURVED_ARROW = 'curvedArrow'
const curvedNoSpread = { ...pathCurved(), spread: undefined }

export const GRAPH_PRIMITIVES = {
  edges: {
    paths: [pathLine(), curvedNoSpread, pathLoop()],
    extremities: [extremityArrow()],
  },
}

export function graphStyles<N extends Attributes, E extends Attributes>(
  look: () => GraphLook,
): StylesDeclaration<N, E> {
  return {
    nodes: [
      DEFAULT_STYLES.nodes,
      {
        labelFont: LABEL_FONT,
        labelSize: LABEL_SIZE,
        labelColor: (_a, s) => (isLit(s) ? look().palette.hoverLabel : look().palette.labelColor),
        labelVisibility: (_a, s) => (isLit(s) ? 'visible' : look().showLabels ? 'auto' : 'hidden'),
        depth: (_a, s) => (isLit(s) ? 'topNodes' : 'nodes'),
        backdropVisibility: (_a, s) => (isLit(s) ? 'visible' : 'hidden'),
        backdropColor: () => look().palette.hoverBg,
        backdropBorderColor: () => look().palette.hoverStroke,
        backdropBorderWidth: 1,
        backdropShadowColor: '#000',
        backdropShadowBlur: 8,
        backdropPadding: 2,
      },
    ],
    edges: [
      DEFAULT_STYLES.edges,
      {
        color: (a) => (typeof a.color === 'string' ? a.color : look().palette.edgeColor),
        path: (a) => (a.type === CURVED_ARROW ? 'curved' : 'straight'),
        head: (a) => (a.type === CURVED_ARROW ? 'arrow' : 'none'),
      },
    ],
  }
}
