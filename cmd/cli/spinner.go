// spinner.go — constants for the braille loading indicator the
// renderer paints while a long-running step is in flight.
//
// Both renderers (event_renderer.go for NDJSON, stream_filter.go for
// the client-driven phase 1) share this vocabulary so the visual stays
// consistent across phases.
//
// Brand kit page 9 specifies the canonical pattern: 8 frames at 80 ms
// each, in mint-400. We tried inline-image (GIF) rendering for
// iTerm2/kitty as a richer alternative, but multi-row images don't
// repaint cleanly with single-line `\r\x1b[2K` overprint — every tick
// stacks a new frame vertically. Ripped out. Braille-only is the right
// answer: works in every TTY, no protocol detection needed, no risk of
// visual artefacts on edge-case terminals.

package main

// brailleFrames is the canonical voodu CLI loading pattern from brand
// kit page 9. 8 frames at 80 ms each = one full revolution in ~640ms,
// which reads as "active, moving" without being distracting.
const brailleFrames = "⠂⠒⠚⠞⠟⠏⠇⠃"

// brailleTickMS is the frame-advance cadence. The brand kit specifies
// 80 ms/frame — slow enough that individual frames are legible, fast
// enough that the motion feels alive.
const brailleTickMS = 80
