go test -count=1 -run TestEveryPythonScriptCompilesWithWarningsAsErrors -v ./scripts/ > /tmp/claude-501/r21.log 2>&1
rc=$?
grep -E '^---|does not compile|invalid escape|compile with' /tmp/claude-501/r21.log
echo "go test exit=$rc"
exit $rc
