# Agent Control v0.15.4

## ZCode preflight under host load

ZCode's manifest grants its Electron-based `--version` probe 30 seconds, while
other adapters retain the five-second version-probe timeout. Launch and doctor
use the timeout declared by the adapter manifest. Probe timeouts identify the
adapter, timeout, and elapsed time, and note that host overload may have delayed
the CLI. Launch probes remain fresh because the runtime has no reusable native
version-probe cache.
