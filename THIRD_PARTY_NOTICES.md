# Third-party notices

Birdsense is built on work by others. This file lists what it uses under a
licence that asks for credit, and ships in the container image alongside it.

## BirdNET

Birdsense identifies bird calls with **BirdNET**, developed by the
[K. Lisa Yang Center for Conservation Bioacoustics](https://www.birds.cornell.edu/ccb/)
at the Cornell Lab of Ornithology and
[Chemnitz University of Technology](https://www.tu-chemnitz.de/). Project home:
<https://birdnet.cornell.edu/>.

- **BirdNET models** (the v2.4 acoustic and geo models, downloaded into the
  image at build time; see `Dockerfile`) are licensed under the
  [Creative Commons Attribution-NonCommercial-ShareAlike 4.0 International License (CC BY-NC-SA 4.0)](https://creativecommons.org/licenses/by-nc-sa/4.0/).
  The full licence text is in [LICENSES/CC-BY-NC-SA-4.0.txt](LICENSES/CC-BY-NC-SA-4.0.txt).
  The models are used as published, without modification.
- **The `birdnet` Python package** (`analyzer/requirements.txt`) is licensed
  under the [MIT License](https://github.com/birdnet-team/birdnet/blob/main/LICENSE.md).

The model licence is non-commercial: Birdsense runs BirdNET for Eastside
Audubon Society's volunteer monitoring program, a non-commercial use. Using it
commercially would need a separate licence from the BirdNET team.

Citation, as the BirdNET team asks:

> Kahl, S., Wood, C. M., Eibl, M., & Klinck, H. (2021). BirdNET: A deep
> learning solution for avian diversity monitoring. *Ecological Informatics*,
> 61, 101236. <https://doi.org/10.1016/j.ecoinf.2021.101236>

## Perch

When the server runs Perch as a second step (`BIRDSENSE_PERCH`), it uses
**Perch 2.0**, Google's bird vocalization classifier, developed by Google
DeepMind and published on
[Kaggle Models](https://www.kaggle.com/models/google/bird-vocalization-classifier)
(the `perch_v2_cpu` variation).

- **The Perch v2 model** (downloaded into the image at build time by the
  `birdnet` package, which fetches its own copy of that release; see
  `Dockerfile`) is licensed under the
  [Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0). The full
  licence text is in [LICENSES/Apache-2.0.txt](LICENSES/Apache-2.0.txt). The
  model is used as published, without modification. Its labels are scientific
  names; the common names Birdsense shows beside them are BirdNET's.
- **TensorFlow** (`analyzer/requirements-perch.txt`), which Perch runs on, is
  also licensed under the Apache License 2.0.

## libflac.js and libFLAC

The browser encodes a card's WAV recordings as FLAC before sending them with
**libflac.js** 5.6.0 (<https://github.com/mmig/libflac.js>), a WebAssembly
build of the Xiph.Org Foundation's **libFLAC** 1.3.4 (<https://xiph.org/flac/>)
with **libogg** 1.3.6 (<https://xiph.org/ogg/>). It is vendored, unmodified,
in `frontend/vendor/libflacjs-5.6.0/`, and served from there:

| File | sha256 |
|---|---|
| `libflac.min.wasm.js` | `76892e974c95adc489910bc991e1d234bb48fb9f1bcc3f76ab471a1493c30f7e` |
| `libflac.min.wasm.wasm` | `dd145880539e836501e3c779aae19e0d3915a90fbf83b50c8a5cde819c6450b0` |

Both are the package's own `dist/` files, as npm publishes `libflacjs@5.6.0`
and jsDelivr serves them at
`https://cdn.jsdelivr.net/npm/libflacjs@5.6.0/dist/`; checking an update is
comparing those hashes. Upgrading means replacing the directory, and the
version in `js/flac-worker.js` and `js/flac.js` (`ENCODER`) with it.

- **libflac.js** is licensed under the MIT License:
  [LICENSES/MIT-libflac.js.txt](LICENSES/MIT-libflac.js.txt).
- **libFLAC** is licensed under the BSD 3-Clause License:
  [LICENSES/BSD-3-Clause-libFLAC.txt](LICENSES/BSD-3-Clause-libFLAC.txt).
- **libogg** is licensed under the BSD 3-Clause License:
  [LICENSES/BSD-3-Clause-libogg.txt](LICENSES/BSD-3-Clause-libogg.txt).

## Banner photo

`frontend/images/barred-owl.jpg`: "Barred Owl forest canopy Seattle Washington
2026" by Guywelch2000, from
[Wikimedia Commons](https://commons.wikimedia.org/wiki/File:Barred_Owl_forest_canopy_Seattle_Washington_2026.jpg),
licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
Resized for the web; otherwise unmodified. It is the banner on the home and
sign-in pages, and the credit is shown on the photo in both
(`<bs-home-page>`, `<bs-signin-page>`).
