# pgswitch relay, for a Compose service beside the app. goreleaser builds it
# with the static Linux binaries under $TARGETPLATFORM.
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pgswitch /pgswitch
# The relay listens on 5432 like Postgres; serve skips containers with this
# label when it looks for a repository's Postgres in Docker.
LABEL io.github.mickamy.pgswitch.relay=true
EXPOSE 5432
ENTRYPOINT ["/pgswitch"]
CMD ["relay"]
