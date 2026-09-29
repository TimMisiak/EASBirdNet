# The BirdNET runtime: Python, the pinned venv and the models, and nothing of
# Birdsense's own. It is the base of the main Dockerfile's `analyzer` target,
# built on its own because it is most of that image's size and build time and
# changes only when the requirements do. scripts/deploy.ps1 tags it with a hash
# of this file and the two requirements files, and builds it only when the
# registry doesn't have that tag yet; compose builds it as the `birdnet`
# service. Its build context is this directory, and it copies only the
# requirements, so editing analyze.py or clip.py never rebuilds it.
#
# BirdNET runs from the `birdnet` Python package on LiteRT (no TensorFlow).
# Its wheels are built for glibc, which is why the runtime is Debian, not
# Alpine. Keep this Python version in step with requirements.txt.
# Perch, the optional second model (BIRDSENSE_PERCH), only runs on TensorFlow,
# which requirements-perch.txt adds. It is always installed, so turning Perch
# on is a setting rather than a different image -- at ~1.3 GB of TensorFlow
# and ~400 MB of model on top of everything else (DEPLOYMENT.md).
FROM python:3.12-slim-bookworm

ENV PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    BIRDNET_APP_DATA=/opt/birdnet/models

RUN python -m venv /opt/birdnet/venv
COPY requirements.txt /opt/birdnet/requirements.txt
RUN /opt/birdnet/venv/bin/pip install -r /opt/birdnet/requirements.txt
COPY requirements-perch.txt /opt/birdnet/requirements-perch.txt
RUN /opt/birdnet/venv/bin/pip install -r /opt/birdnet/requirements.txt -r /opt/birdnet/requirements-perch.txt

# Download the models into the image (acoustic, plus geo for location
# filtering, plus Perch) so analysis never reaches the network. The loads must
# match the ones in analyze.py.
RUN /opt/birdnet/venv/bin/python -c 'import birdnet; \
birdnet.load("acoustic", "2.4", "tf", library="litert"); \
birdnet.load("geo", "2.4", "tf", library="litert"); \
birdnet.load_perch_v2()'
