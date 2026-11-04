# idp-workspace image: one static binary and the built web shell on scratch. Built by
# deploy/build.sh from artifacts compiled on the host, with --network none, so no base image or
# package index is needed. Runs as uid 10001; works with a read-only root.
FROM scratch
# No --chmod here: BuildKit applies it to the created parent directories too, and a 0644 /etc is not
# traversable by uid 10001. The build context carries the right modes instead (deploy/build.sh).
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY web /usr/share/idp-workspace/web
COPY --chmod=0755 idp-workspace /usr/local/bin/idp-workspace
ENV IDP_WORKSPACE_WEB_DIR=/usr/share/idp-workspace/web
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/idp-workspace"]
CMD ["serve"]
