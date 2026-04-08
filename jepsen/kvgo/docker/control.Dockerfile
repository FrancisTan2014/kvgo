FROM eclipse-temurin:21-jdk-jammy

# Leiningen + tools
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        openssh-client \
        curl \
        gnuplot \
        git && \
    rm -rf /var/lib/apt/lists/*

# Install Leiningen
RUN curl -fsSL https://raw.githubusercontent.com/technomancy/leiningen/stable/bin/lein \
        -o /usr/local/bin/lein && \
    chmod +x /usr/local/bin/lein && \
    lein version

# SSH config: accept any host key (test-only, never production)
RUN mkdir -p /root/.ssh && \
    echo 'Host *\n  StrictHostKeyChecking no\n  UserKnownHostsFile /dev/null' \
        > /root/.ssh/config

# SSH key pair for passwordless auth to db nodes
COPY docker/secret/id_rsa     /root/.ssh/id_rsa
COPY docker/secret/id_rsa.pub /root/.ssh/id_rsa.pub
RUN chmod 600 /root/.ssh/id_rsa

WORKDIR /jepsen/kvgo

# Pre-fetch Leiningen dependencies (cached in image layer).
# Copy only project.clj first so deps layer is invalidated only when
# dependencies change, not on every source edit.
COPY project.clj /jepsen/kvgo/project.clj
RUN lein deps

CMD ["bash"]
