#!/usr/bin/env bash
# Score a bench run against the acceptance thresholds.
#
# These are development thresholds, used to catch a regression between changes.
# They are not a pre-registered benchmark and the numbers they print are not
# citeable as a headline result.
#
# The thresholds:
#   - the four recovery scenarios must score 100%
#   - us_per_write must be 25 or less
#   - overhead_pct and boundary_ms are REPORTED, not gated
#
# Reads results/bench.json, which `make bench` produces.
set -euo pipefail

RESULTS="${1:-results/bench.json}"
[ -f "$RESULTS" ] || { echo "ACCEPT FAIL: $RESULTS missing (run make bench)"; exit 1; }

python3 - "$RESULTS" <<'EOF'
import json, sys

r = json.load(open(sys.argv[1]))
agg = r["aggregate"]
fails = []

def gate(cond, msg):
    if not cond:
        fails.append(msg)

for name in ("rm_rf_restore_pct", "transient_salvage_pct",
             "human_preservation_pct", "agent_delete_undo_pct"):
    gate(agg.get(name) == 100, f"{name} {agg.get(name)} != 100")
gate(agg.get("us_per_write", 1e9) <= 25.0,
     f"us_per_write {agg.get('us_per_write')} > 25")

print(f"reported (not gated): overhead_pct={agg.get('overhead_pct')} "
      f"boundary_ms={agg.get('boundary_ms')} routine_cut_ms={agg.get('routine_cut_ms')} "
      f"feed_active={agg.get('feed_active')}")

# Overhead on the realistic compile workload, against a 5% bar. Informational
# only: promoting it into the gated set is a one-line change, adding
# `gate(rp is not None and rp < 5.0, ...)` below.
rp = agg.get("overhead_realistic_pct")
if rp is None:
    print("realistic overhead vs the 5% bar: SKIPPED (no gcc)")
else:
    verdict = "OK" if rp < 5.0 else "EXCEEDED"
    print(f"realistic overhead {rp:.2f}% "
          f"(unwrapped {agg.get('realistic_unwrapped_ms')} ms) "
          f"vs the 5% bar: {verdict}")
if fails:
    for f in fails:
        print("ACCEPT FAIL:", f)
    sys.exit(1)
print("ACCEPT PASS (development thresholds only, not a headline benchmark)")
EOF
