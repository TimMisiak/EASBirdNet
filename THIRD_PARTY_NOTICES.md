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

## Banner photo

`frontend/images/barred-owl.jpg`: "Barred Owl forest canopy Seattle Washington
2026" by Guywelch2000, from
[Wikimedia Commons](https://commons.wikimedia.org/wiki/File:Barred_Owl_forest_canopy_Seattle_Washington_2026.jpg),
licensed under [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/).
Resized for the web; otherwise unmodified. It is the banner on the home and
sign-in pages, and the credit is shown on the photo in both
(`<bs-home-page>`, `<bs-signin-page>`).
