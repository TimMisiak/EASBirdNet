"""Writes BirdNET's species list as the JSON the Review rules page looks names up in.

    python analyzer/species.py > frontend/data/birdnet-species.json

Every class the v2.4 acoustic model can report, as [scientific, common] pairs:
birds, and a few that aren't (Dog, Engine, Human vocal). The scientific names
are spelled as BirdNET spells them, which is what a rule has to match. Run it
again when the model analyze.py loads changes.
"""

import json
import sys


def main():
    import birdnet

    model = birdnet.load("acoustic", "2.4", "tf", library="litert")
    species = sorted(str(label).partition("_")[::2] for label in model.species_list)
    about = (
        "Every class BirdNET's v2.4 acoustic model reports, as [scientific name, common name], "
        "from analyzer/species.py. CC BY-NC-SA 4.0, as the model is (THIRD_PARTY_NOTICES.md)."
    )
    # One species a line, so a new model's list is a readable diff.
    rows = ",\n".join("  " + json.dumps(list(pair), ensure_ascii=False) for pair in species)
    sys.stdout.write(f'{{"about": {json.dumps(about)},\n "model": "BirdNET v2.4",\n "species": [\n{rows}\n]}}\n')


if __name__ == "__main__":
    main()
