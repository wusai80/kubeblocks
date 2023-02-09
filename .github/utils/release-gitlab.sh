#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

DEFAULT_PACKAGE_NAME=kubeblocks
DEFAULT_CHANNEL=stable
API_URL=https://jihulab.com/api/v4/projects

show_help() {
cat << EOF
Usage: $(basename "$0") <options>

    -h, --help                Display help
    -t, --type                Release operation type
                                1) create release
                                2) upload release asset
                                3) release helm chart
    -tn, --tag-name           Release tag name
    -pi, --project-id         Gitlab repo project id or "group%2Fproject"
    -at, --access-token       Gitlab access token
    -au, --access-user        Gitlab access username
    -ap, --asset-path         Upload asset file path
    -an, --asset-name         Upload asset file name
    -pn, --package-name       Gitlab package name (default: $DEFAULT_PACKAGE_NAME)
    -c, --channel             Gitlab helm channel name (default: DEFAULT_CHANNEL)
EOF
}

main() {
    local PACKAGE_NAME=$DEFAULT_PACKAGE_NAME
    local CHANNEL=$DEFAULT_CHANNEL
    local TAG_NAME
    local PROJECT_ID
    local ACCESS_TOKEN
    local ACCESS_USER
    local ASSET_PATH
    local ASSET_NAME

    parse_command_line "$@"

    if [[ $TYPE == 1 ]]; then
        create_release
    elif [[ $TYPE == 2 ]]; then
        upload_asset
        update_release_asset
    elif [[ $TYPE == 3 ]]; then
        release_helm
    fi

}

parse_command_line() {
    while :; do
        case "${1:-}" in
            -h|--help)
                show_help
                exit
                ;;
            -t|--type)
                if [[ -n "${2:-}" ]]; then
                    TYPE="$2"
                    shift
                fi
                ;;
            -t|--tag-name)
                if [[ -n "${2:-}" ]]; then
                    TAG_NAME="$2"
                    shift
                else
                    echo "ERROR: '-t|--tag-name' cannot be empty." >&2
                    show_help
                    exit 1
                fi
                ;;
            -pi|--project-id)
                if [[ -n "${2:-}" ]]; then
                    PROJECT_ID="$2"
                    shift
                else
                    echo "ERROR: '-pi|--project-id' cannot be empty." >&2
                    show_help
                    exit 1
                fi
                ;;
            -at|--access-token)
                if [[ -n "${2:-}" ]]; then
                    ACCESS_TOKEN="$2"
                    shift
                else
                    echo "ERROR: '-at|--access-token' cannot be empty." >&2
                    show_help
                    exit 1
                fi
                ;;
            -at|--access-user)
                if [[ -n "${2:-}" ]]; then
                    ACCESS_USER="$2"
                    shift
                fi
                ;;
            -ap|--asset-path)
                if [[ -n "${2:-}" ]]; then
                    ASSET_PATH="$2"
                    shift
                fi
                ;;
            -ap|--asset-name)
                if [[ -n "${2:-}" ]]; then
                    ASSET_NAME="$2"
                    shift
                fi
                ;;
            -pn|--package-name)
                if [[ -n "${2:-}" ]]; then
                    PACKAGE_NAME="$2"
                    shift
                fi
                ;;
            -c|--channel)
                if [[ -n "${2:-}" ]]; then
                    CHANNEL="$2"
                    shift
                fi
                ;;
            *)
                break
                ;;
        esac

        shift
    done

    if [[ -z "$PROJECT_ID" ]]; then
        echo "ERROR: '-pi|--project-id' is required." >&2
        show_help
        exit 1
    fi

    if [[ -z "$ACCESS_TOKEN" ]]; then
        echo "ERROR: '-at|--access-token' is required." >&2
        show_help
        exit 1
    fi
}

gitlab_api_curl() {
    curl --header "Authorization: Bearer $ACCESS_TOKEN" \
      --header 'Content-Type: application/json' \
      $@
}

create_release() {
    request_type=POST
    request_url=$API_URL/$PROJECT_ID/releases
    request_data='{"ref":"main","name":"KubeBlocks\t'$TAG_NAME'","tag_name":"'$TAG_NAME'"}'

    gitlab_api_curl --request $request_type $request_url --data $request_data
}

upload_asset() {
    request_url=$API_URL/$PROJECT_ID/packages/generic/$PACKAGE_NAME/$TAG_NAME/

    gitlab_api_curl $request_url --upload-file $ASSET_PATH
}

update_release_asset() {
    request_type=POST
    request_url=$API_URL/$PROJECT_ID/releases/$TAG_NAME/assets/links
    asset_url=$API_URL/$PROJECT_ID/packages/generic/$PACKAGE_NAME/$TAG_NAME/$ASSET_NAME
    request_data='{"url":"'$asset_url'","name":"'$ASSET_NAME'","link_type":"package"}'

    gitlab_api_curl --request $request_type $request_url --data $request_data
}

release_helm() {
    request_type=POST
    request_url=$API_URL/$PROJECT_ID/packages/helm/api/$CHANNEL/charts

    curl --request $request_type $request_url --form 'chart=@'$ASSET_PATH --user $ACCESS_USER:$ACCESS_TOKEN
}

main "$@"