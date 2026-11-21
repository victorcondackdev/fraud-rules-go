# syntax=docker/dockerfile:1

FROM golang:1.24 AS build
WORKDIR /src
# Standard library only: no module download step.
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/riskd ./cmd/riskd

# distroless/static ships CA certificates and a nonroot user; tzdata is
# embedded in the binary, so the image needs nothing else.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/riskd /riskd
COPY rulesets/ /rulesets/
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/riskd", "-addr", ":8080", "-ruleset", "/rulesets/pix-default.json"]
