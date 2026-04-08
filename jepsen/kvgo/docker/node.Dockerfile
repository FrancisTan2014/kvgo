FROM debian:trixie-slim

# SSH server + system tools Jepsen needs
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        openssh-server \
        iptables \
        iproute2 \
        iputils-ping \
        curl \
        procps \
        psmisc \
        sudo && \
    rm -rf /var/lib/apt/lists/*

# SSH config: allow root login with password + key
RUN mkdir -p /run/sshd /root/.ssh && \
    echo 'root:root' | chpasswd && \
    sed -i 's/#PermitRootLogin.*/PermitRootLogin yes/' /etc/ssh/sshd_config && \
    sed -i 's/#PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config

# Authorized key from control node
COPY docker/secret/id_rsa.pub /root/.ssh/authorized_keys
RUN chmod 600 /root/.ssh/authorized_keys

# kv-server binary (cross-compiled for linux/amd64)
COPY bin/kv-server /opt/kvgo/kv-server
RUN chmod +x /opt/kvgo/kv-server

EXPOSE 22 4000 5000 8080

CMD ["/usr/sbin/sshd", "-D"]
