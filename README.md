# pisafe

`pisafe` is an implementation in progress of the isolation model described in
[`pisafe-design.md`](pisafe-design.md).

The first implementation slice contains:

- a dependency-free Go controller;
- `pisafe doctor` for host prerequisite checks;
- a split Git staging core: the Mac produces a bundle and tracked-state patch,
  while materialization happens after transfer inside the isolated
  environment;
- tracked dirty-state baseline capture;
- final tracked-state capture;
- split apply preparation/import, with SHA-256 verification and a
  compare-and-swap update of a new `pisafe/<run>` branch; and
- tests proving workspace deletion and apply do not modify the source checkout.

The run lifecycle is not exposed yet. It needs the Lima/SSH transport and the
submodule-aware, journaled multi-repository apply protocol first.

## Development

```sh
go test ./...
go build ./cmd/pisafe
./pisafe doctor
```
