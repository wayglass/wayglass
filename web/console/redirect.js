// Preserve the project, never arbitrary API hosts or credential parameters.
const project = new URL(location.href).searchParams.get("project");
const target = new URL("/", location.origin);
if (/^[a-z0-9][a-z0-9._-]{0,62}$/.test(project || ""))
  target.searchParams.set("project", project);
target.hash = project ? "/review" : "/signin";
location.replace(target);
