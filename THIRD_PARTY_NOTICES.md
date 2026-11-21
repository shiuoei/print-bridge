# Third-Party Notices

This project includes or may distribute third-party components under their own
licenses.

## Reference implementation (not bundled)

PrintBridge is an independent Go implementation whose protocol and feature
scope are modelled on the PrintBridge project:

- Project: PrintBridge
- Source: https://github.com/vergil-lai/print-bridge

No source code from that project is included in this repository or its release
artifacts. Its public technical documentation was used as the protocol reference
for compatibility.

## SumatraPDF

PrintBridge includes the Windows portable SumatraPDF executable for PDF
printing:

```text
resources/windows-amd64/SumatraPDF.exe
```

- Project: SumatraPDF
- Website: https://www.sumatrapdfreader.org/
- Source code: https://github.com/sumatrapdfreader/sumatrapdf
- License: GNU General Public License version 3
- Bundled version: 3.6.1 64-bit portable
- ZIP SHA-256: `98b33a518d42986856d225064b0cd2d3643ecf78cbf84ab873d26cc51877a544`
- EXE SHA-256: `719f689b34f47be8ca105ce8484948474dafde0e106bab599e4a89326070c3d0`

SumatraPDF is not covered by this project's Apache License 2.0. Its own license
terms apply when redistributing binaries that include it.
