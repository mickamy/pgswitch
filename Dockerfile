# pgswitch relay, for a Compose service beside the app. goreleaser builds it
# with the static Linux binaries under $TARGETPLATFORM.
FROM scratch
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/pgswitch /pgswitch
EXPOSE 5432
ENTRYPOINT ["/pgswitch"]
CMD ["relay"]
