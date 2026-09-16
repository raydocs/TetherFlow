# Cloud Agent base image for TetherFlow.
#
# Provides the stable, slow-changing system toolchains needed to build the
# project end to end:
#   - JDK 17            -> Gradle build + Kotlin/Compose Android app
#   - Android SDK 36    -> compileSdk/targetSdk 36, build-tools 36.0.0
#   - Go 1.22           -> cross-platform desktop companion (desktop/)
#
# Repository-dependent work (dependency download, compilation) lives in the
# `install` command in .cursor/environment.json, not here.

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Base system packages. git + curl are required in fully custom images.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        openjdk-17-jdk-headless \
        ca-certificates \
        curl \
        unzip \
        zip \
        git \
        build-essential \
    && rm -rf /var/lib/apt/lists/*

# --- Java ---
ENV JAVA_HOME=/usr/lib/jvm/java-17-openjdk-amd64
ENV PATH="${JAVA_HOME}/bin:${PATH}"

# --- Go (matches desktop/go.mod: go 1.22) ---
ARG GO_VERSION=1.22.12
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" -o /tmp/go.tgz \
    && rm -rf /usr/local/go \
    && tar -C /usr/local -xzf /tmp/go.tgz \
    && rm /tmp/go.tgz
ENV PATH="/usr/local/go/bin:${PATH}"

# --- Android SDK ---
ENV ANDROID_HOME=/opt/android-sdk
ENV ANDROID_SDK_ROOT=/opt/android-sdk
ARG ANDROID_CMDLINE_TOOLS_VERSION=11076708
ARG ANDROID_PLATFORM=android-36
ARG ANDROID_BUILD_TOOLS=36.0.0
RUN mkdir -p "${ANDROID_HOME}/cmdline-tools" \
    && curl -fsSL "https://dl.google.com/android/repository/commandlinetools-linux-${ANDROID_CMDLINE_TOOLS_VERSION}_latest.zip" -o /tmp/cmdline-tools.zip \
    && unzip -q /tmp/cmdline-tools.zip -d /tmp/cmdline-tools-extract \
    && mv /tmp/cmdline-tools-extract/cmdline-tools "${ANDROID_HOME}/cmdline-tools/latest" \
    && rm -rf /tmp/cmdline-tools.zip /tmp/cmdline-tools-extract
ENV PATH="${ANDROID_HOME}/cmdline-tools/latest/bin:${ANDROID_HOME}/platform-tools:${PATH}"
RUN yes | sdkmanager --licenses > /dev/null \
    && sdkmanager \
        "platform-tools" \
        "platforms;${ANDROID_PLATFORM}" \
        "build-tools;${ANDROID_BUILD_TOOLS}" \
    && chmod -R a+rwX "${ANDROID_HOME}"

# Run as the pre-existing non-root ubuntu user (uid 1000).
USER ubuntu
WORKDIR /home/ubuntu
