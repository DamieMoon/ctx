//go:build integration

// The W5/K1-1 red probes: the label-expression shapes that exist ONLY so
// topic_label_integration_test.go can patch them into fallbackLabelTemplate and
// watch a guard fire (design/01 §7 W5, §4.6).
//
// Why they live in a test file: they are instruments, not production shapes —
// nothing in label.go renders them, and the only readers are the two probes in
// topic_label_integration_test.go. As package-level vars in label.go they read
// as dead to `unused` on every tag-less lint run (CI, .hooks/pre-commit), which
// is why they used to carry three //nolint:unused annotations. Here the
// annotation is unnecessary: the file compiles under the same build tag as its
// readers, so the linter sees a use whenever it sees the declaration and never
// sees one without the other. Same package as label.go, so the stage constants
// and sqlStage stay reachable without exporting anything.
package overview

// fallbackLabelExprUncapped drops the 120-rune cap: the shape that lets an
// unbounded tag break gct_label_len inside the persist transaction. Read by
// topic_label_integration_test.go (G1b).
var fallbackLabelExprUncapped = `COALESCE(
             ` + sqlStage(fallbackTagStage) + `,
             ` + sqlStage(fallbackCategoryStage) + `,
             ` + sqlStage(fallbackTitleStage) + `,
             '` + fallbackLastResort + `'
           )`

// fallbackTagStageLegacy is the pre-K1-1 tag rung: emptiness tested on the RAW
// token with btrim/1, which only knows U+0020. Read only through
// fallbackLabelExprLegacy below.
var fallbackTagStageLegacy = `(SELECT string_agg(x.tg, ' · ' ORDER BY x.cnt DESC, x.tg)
                 FROM (SELECT tg.tg, count(*) AS cnt
                         FROM unnest(n.core_blocks) AS cb
                         JOIN context_blocks b ON b.id = cb AND b.scope = n.scope
                          AND b.sensitivity IN ('internal','public')
                         CROSS JOIN LATERAL unnest(b.tags) AS tg(tg)
                        WHERE btrim(tg.tg) <> ''
                        GROUP BY tg.tg
                        ORDER BY count(*) DESC, tg.tg
                        LIMIT 3) x)`

// fallbackLabelExprLegacy is the pre-K1-1 shape, kept for the negative probe:
// the emptiness tests run on the RAW text, the normalisation runs after the
// COALESCE has already committed to a stage, and the constant sits INSIDE the
// COALESCE where a blank-but-not-NULL stage skips right past it. Read by
// topic_label_integration_test.go (K1-1).
var fallbackLabelExprLegacy = `btrim(left(btrim(regexp_replace(COALESCE(
             ` + fallbackTagStageLegacy + `,
             ` + fallbackCategoryStage + `,
             nullif(btrim(n.repr_title), ''),
             '` + fallbackLastResort + `'
           ), '\s+', ' ', 'g')), 120))`
