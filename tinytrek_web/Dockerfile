# Specify Python base image, build argument enables cross-platform builds
ARG PYTHON_VERSION=3.12
ARG DEBIAN_FRONTEND=noninteractive

FROM python:${PYTHON_VERSION}-slim

# Install required packages for cross-compilation
RUN apt-get update && \
    apt-get install -yq build-essential curl && \
    rm -rf /var/lib/apt-lists/* && \
    curl -sSL https://install.python-poetry.org | python3 -

# Install dependencies using Poetry
COPY pyproject.toml poetry.lock ./
RUN /root/.local/bin/poetry install --no-root

# Copy the project code
COPY . .

# Start the application
CMD ["/root/.local/bin/poetry", "run", "python", "tinytrek.py"]
