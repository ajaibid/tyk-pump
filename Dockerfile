# Runtime-only image. The tyk-pump binary is compiled ON THE RUNNER by
# .github/workflows/build-and-push.yaml — Go modules are pulled through the GAR
# Go proxy via a Workload-Identity .netrc (the DMZ perimeter cannot reach the old
# Athens proxy). The prebuilt static binary is then COPYd in here, so this Docker
# build fetches nothing and needs no module auth.
FROM gcr.io/distroless/static-debian11

COPY dist/tyk-pump /opt/tyk-pump/tyk-pump
WORKDIR /opt/tyk-pump
CMD ["/opt/tyk-pump/tyk-pump"]
