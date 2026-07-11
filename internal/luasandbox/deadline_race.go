//go:build race

package luasandbox

// CallDeadline under the race detector. `-race` instruments every VM op, making Lua ~10x slower, so a
// template that is comfortably sub-millisecond (and well within the 100k instruction budget) in production
// can still take a few ms under -race. Scaling the WALL-CLOCK deadline up in the race build keeps CI from
// tripping the secondary stall guard on legitimately-bounded content, WITHOUT touching the production 5ms
// (this file is compiled only under -race). The instruction budget — the real per-call bound — is unchanged.
const CallDeadline = 50
