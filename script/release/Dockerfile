FROM golang:1.17.7 AS build

# Install acceld
ADD ./ /accel
RUN make -C /accel install-check-tools
RUN make -C /accel

# Install containerd
RUN wget https://github.com/containerd/containerd/releases/download/v1.5.7/containerd-1.5.7-linux-amd64.tar.gz
RUN mkdir -p /usr/local/bin/ && tar xzf containerd-1.5.7-linux-amd64.tar.gz -C /usr/local/
RUN ls -l /usr/local/bin/

# Build acceld image
FROM photon:4.0
COPY --from=build /accel/acceld /accel/accelctl /usr/local/bin/containerd /usr/local/bin/
RUN mkdir /etc/containerd && /usr/local/bin/containerd config default > /etc/containerd/config.toml
ADD ./script/release/entrypoint.sh /entrypoint.sh

ENTRYPOINT [ "/entrypoint.sh" ]
