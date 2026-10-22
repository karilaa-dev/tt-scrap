# Prepared image reference outputs

`image-golden.json` records source hashes, prepared JPEG hashes, byte counts, and
dimensions produced by commit `7ea514022336776215c043aa44a507f47e97a01d` with Go
1.27.2 and native HEIF decoding. Inputs are the existing `sample.heic` and
`static.webp` fixtures in `internal/media/testdata`.

The image delivery benchmark verifies the original source and exact output.
Do not regenerate these hashes from a candidate optimization merely to make a
comparison pass; investigate any output difference first.
