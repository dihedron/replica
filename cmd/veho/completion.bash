# veho completion: source it, e.g. source <(veho completion bash)
if [ -n "$ZSH_VERSION" ]; then autoload -U +X bashcompinit && bashcompinit; fi
_veho() {
	local args=("${COMP_WORDS[@]:1:$COMP_CWORD}")
	local IFS=$'\n'
	COMPREPLY=($(GO_FLAGS_COMPLETION=1 "${COMP_WORDS[0]}" "${args[@]}"))
	return 0
}
complete -F _veho veho
