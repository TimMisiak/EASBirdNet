"""Run BirdNET over audio files and print the detections as JSON.

This is the only Python in Birdsense. The Go server runs it as a subprocess
(backend/internal/birdnet) rather than linking a model runtime into the binary,
because BirdNET's maintained runtime is the `birdnet` Python package.

    python analyze.py [options] FILE...

`--model perch` runs Google's Perch v2 instead of BirdNET: a second opinion on
the same audio, not a replacement (backend/internal/analysis runs it as a step
of its own after BirdNET). Perch scores 5-second windows rather than 3, only
runs on TensorFlow (requirements-perch.txt), and labels its classes with
scientific names alone, so common names are borrowed from BirdNET's labels.

stdout carries exactly one JSON object, written when the run finishes:

    {"model": "BirdNET_GLOBAL_6K_V2.4",
     "files": [{"path": "/cards/a.wav", "detections": [
                  {"startSec": 0, "endSec": 3, "scientificName": "Pandion haliaetus",
                   "commonName": "Osprey", "confidence": 0.997}]},
               {"path": "/cards/b.wav", "error": "unreadable audio"}]}

Files are reported in the order given. A file that can't be analyzed carries
`error` instead of failing the run; anything else that goes wrong exits non-zero
with the reason on stderr. Logs and progress also go to stderr.

Models are stored under $BIRDNET_APP_DATA and downloaded on first use. The Docker
image downloads them at build time, so analysis there never needs the network.
"""

import argparse
import json
import logging
import os
import sys
from pathlib import Path

MODEL_NAMES = {"birdnet": "BirdNET_GLOBAL_6K_V2.4", "perch": "Perch_v2"}


def main() -> int:
    args = parse_args()
    # Everything but the final JSON goes to stderr, including the birdnet
    # package's own logging and tqdm download bars.
    logging.basicConfig(stream=sys.stderr, level=logging.INFO)

    import birdnet

    # LiteRT runs the TFLite model without TensorFlow, which would add ~600 MB.
    # It is loaded for Perch too, for its labels: they are how Perch's
    # scientific names get common ones.
    acoustic = birdnet.load("acoustic", "2.4", "tf", library="litert")
    common_names = dict(str(label).partition("_")[::2] for label in acoustic.species_list)
    species = None
    if args.latitude is not None:
        geo = birdnet.load("geo", "2.4", "tf", library="litert")
        # The geo model scores which species occur at a place (and week), the same
        # filter BirdNET-Analyzer applies with --lat/--lon/--week.
        occurrence = geo.predict(
            args.latitude,
            args.longitude,
            week=args.week,
            min_confidence=args.location_threshold,
        ).to_structured_array(sort_by=None)
        species = [str(name) for name in occurrence["species_name"]]

    if args.model == "perch":
        model = birdnet.load_perch_v2()
        options = {
            # Perch's outputs are logits. A sigmoid over them puts nearly every
            # one of the top five at 0.99 or more, so a threshold means nothing;
            # a softmax spreads one window's score across its classes, which
            # measured 0.33-0.91 for the test Osprey against 0.05 or less for
            # anything else, and so is on the scale --min-confidence expects.
            "apply_softmax": True,
        }
        if species is not None:
            # The geo model speaks BirdNET's "Scientific_Common" labels; Perch
            # has scientific names only, and refuses a name it doesn't have.
            expected = {name.partition("_")[0] for name in species}
            species = [name for name in model.species_list if name in expected]
    else:
        model = acoustic
        options = {"sigmoid_sensitivity": args.sensitivity}

    # birdnet rejects the whole batch if one input is missing, so check each file
    # first and report the bad ones individually.
    results = {path: {"path": path, "detections": []} for path in args.files}
    runnable = []
    for path in results:
        if not Path(path).is_file():
            results[path] = {"path": path, "error": "file not found"}
        else:
            runnable.append(path)

    if runnable:
        prediction = model.predict(
            runnable,
            top_k=args.top_k,
            n_workers=args.workers,
            overlap_duration_s=args.overlap,
            default_confidence_threshold=args.min_confidence,
            custom_species_list=species,
            **options,
        )
        for row in prediction.to_structured_array():
            scientific, _, common = str(row["species_name"]).partition("_")
            common = common or common_names.get(scientific, "")
            results[str(row["input"])]["detections"].append({
                "startSec": round(float(row["start_time"]), 3),
                "endSec": round(float(row["end_time"]), 3),
                "scientificName": scientific,
                "commonName": common or scientific,
                "confidence": round(float(row["confidence"]), 4),
            })
        # Corrupt or empty audio; birdnet's own unprocessable_inputs are indices.
        for path in prediction.get_unprocessed_files():
            results[str(path)] = {"path": str(path), "error": "unreadable audio"}

    for r in results.values():
        if "detections" in r:
            r["detections"].sort(key=lambda d: (d["startSec"], -d["confidence"]))

    json.dump({"model": MODEL_NAMES[args.model], "files": list(results.values())}, sys.stdout)
    sys.stdout.write("\n")
    return 0


def parse_args() -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("files", nargs="*", help="audio files to analyze")
    p.add_argument("--model", choices=sorted(MODEL_NAMES), default="birdnet")
    p.add_argument("--min-confidence", type=float, default=0.25)
    p.add_argument("--sensitivity", type=float, default=1.0, help="BirdNET only")
    p.add_argument("--overlap", type=float, default=0.0, help="seconds, 0 to <3 (<5 for Perch)")
    p.add_argument("--top-k", type=int, default=5, help="species kept per window")
    p.add_argument("--workers", type=int, default=1, help="inference processes; each loads the model")
    p.add_argument("--latitude", type=float)
    p.add_argument("--longitude", type=float)
    p.add_argument("--week", type=int, help="1-48, four per month; omit for year-round")
    p.add_argument("--location-threshold", type=float, default=0.03)
    args = p.parse_args()
    if (args.latitude is None) != (args.longitude is None):
        p.error("--latitude and --longitude go together")
    if args.week is not None and args.latitude is None:
        p.error("--week needs --latitude and --longitude")
    if not args.files:
        p.error("no files to analyze")
    # Absolute paths are what birdnet reports back, so results can be matched
    # to inputs whatever the working directory.
    args.files = list(dict.fromkeys(os.path.abspath(f) for f in args.files))
    return args


if __name__ == "__main__":
    sys.exit(main())
