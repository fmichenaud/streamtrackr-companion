# Local watch-loop harness

Drives the real companion against a fake Steam tree and a fake API, on any
OS, with no Steam install and no game. It exists because the parts most
likely to be wrong — what happens when a push is still retrying, when
Steam is caught mid-write, when two pushes race — are timing, and unit
tests don't have any.

It found a real bug the unit tests could not: an unreadable stats file
used to fall back to an all-locked baseline, so the first rescan announced
every achievement the player already had.

Two things make this possible: `readSteamPath` honours `$STEAMPATH` off
Windows, and `--cli -appid N` skips the registry-based game detection.

## Setup

```sh
make harness          # builds all three into /tmp/streamtrackr-harness
cd /tmp/streamtrackr-harness
```

Then, in one terminal:

```sh
./fakeapi                        # add -mode fail-unlock | busy-relock | no-route
```

and in another:

```sh
./fakesteam -root ./steam init ACH_A          # ACH_A already unlocked
STEAMPATH=$PWD/steam ./companion --cli -appid 440 -token dev -backend http://127.0.0.1:8799
```

`-token dev` is enough — the fake API never looks at it. Mutate the stats
file from a third terminal with `./fakesteam -root ./steam set …`; every
push shows up in the fakeapi output with the seconds since start.

## Scenarios

Each one is a line to run and a thing to see. They map to the failures
this code was written for.

**1. Nominal — unlock, then reset**

```sh
./fakesteam -root ./steam set ACH_A ACH_B     # earn ACH_B
./fakesteam -root ./steam set ACH_A           # clear it again
```

One `UNLOCK ACH_B`, then one `RELOCK ["ACH_B"]` ~200 ms after the file
changes — the confirmation read.

**2. A reset while a push is still retrying** (`-mode fail-unlock`)

```sh
./fakesteam -root ./steam set ACH_A ACH_B     # the push fails, retry in 3 s
sleep 1
./fakesteam -root ./steam set ACH_A           # reset during the backoff
```

Exactly one `UNLOCK` reaches the API, and the log says `called off 1
unlock push(es) still retrying`. A second `UNLOCK` arriving after the
`RELOCK` is the bug: the server ends up unlocked against a locked Steam,
and that achievement is never announced again.

**3. A stats file that can't be read at startup**

```sh
./fakesteam -root ./steam init ACH_A ACH_B ACH_C
./fakesteam -root ./steam truncate ACH_A ACH_B ACH_C
# start the companion, then a few seconds later:
./fakesteam -root ./steam set ACH_A ACH_B ACH_C
```

Two retries, then a correct `baseline: 3/4 unlocked` and **no push at
all**. Three `UNLOCK`s here means the baseline was taken as all-locked —
the player's whole game announced in chat.

**4. A partial write mid-session**

```sh
./fakesteam -root ./steam truncate ACH_A ACH_B ; sleep 0.4 ; ./fakesteam -root ./steam set ACH_A ACH_B ACH_C
```

`rescan stats: truncated document`, and nothing pushed. A `RELOCKED 3
achievement(s)` here is a reset nobody asked for.

**5. An unrelated unlock while a relock retries** (`-mode busy-relock`)

```sh
./fakesteam -root ./steam set             # re-lock ACH_A, the server answers busy
sleep 1
./fakesteam -root ./steam set ACH_B       # unrelated achievement earned
```

`UNLOCK ACH_B` goes out immediately, while the relock is still on its
second attempt. If it waits for the relock to finish (~9 s), the barrier
has stopped being scoped per achievement.

**6. A server that predates re-locks** (`-mode no-route`)

One `RELOCK`, a 404, no retry, no tray warning. That is what lets a
companion ship before the API does.

## What this does NOT cover

Registry game detection, the tray, the updater, the NSIS installer — all
Windows-only — and how Steam really writes the file (if it writes to a
temporary and renames, the truncation window never opens in practice).
The one test worth doing on real hardware: earn an achievement, clear it
from the Steam console, earn it again, and check it is announced twice.
