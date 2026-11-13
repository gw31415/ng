// Apps started from Finder get a minimal PATH: run ng (and nvim) from a zsh login shell, in $HOME.
#include <libgen.h>
#include <limits.h>
#include <mach-o/dyld.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

int main(int argc, char **argv) {
	char exe[PATH_MAX], ng[PATH_MAX];
	uint32_t size = sizeof exe;
	if (_NSGetExecutablePath(exe, &size) != 0) return 1;
	snprintf(ng, sizeof ng, "%s/ng", dirname(exe));
	char **a = calloc(argc + 4, sizeof *a);
	a[0] = "zsh", a[1] = "-lc", a[2] = "cd && exec \"$0\" \"$@\"", a[3] = ng;
	memcpy(a + 4, argv + 1, (argc - 1) * sizeof *a);
	execv("/bin/zsh", a);
	perror("/bin/zsh");
	return 127;
}
