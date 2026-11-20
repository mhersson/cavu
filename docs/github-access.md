# GitHub access

CAVU acts on GitHub with a GitHub App or a personal access token, fine-grained or classic, and the one you make needs this access to the repository CAVU works on:

| Permission | GitHub App | Fine-grained token | Classic token (scope) | What CAVU does with it |
| --- | --- | --- | --- | --- |
| Contents | Read and write | Read and write | `repo` | Clones the repository and pushes the task's branch |
| Pull requests | Read and write | Read and write | `repo` | Opens the pull request as a draft, reads it with its reviews, marks it ready for review and comments on it |
| Checks | Read-only | Not available (below) | `repo` | Reads the check runs of the pull request's newest commit |
| Commit statuses | Read-only | Read-only | `repo` | Reads the commit statuses of the pull request's newest commit |
| Actions | Read-only | Read-only | `repo` | Reads the end of a failed GitHub Actions job's log for a fix run, and, once GitHub has refused the Checks API, the jobs of the newest commit's workflow runs in place of its check runs |
| Metadata | Read-only | Read-only | `repo` | Reads the repository, and a reviewer's permission on it, which decides [whose requests count](security.md) |
| Workflows | Read and write | Read and write | `workflow` | Only for a change to a file under `.github/workflows/`: pushes it |

CAVU clones the repository with the credential, which a private repository needs, and does everything else in the last column only with a project's switch `publish` on, the default, on a public repository too ([The pull request](tasks.md#the-pull-request)). This is the default credential, which every project uses unless it names another (below).

A GitHub App is recommended: CAVU asks the App's installation on the repository for tokens for that repository alone, each with only what its use needs, and GitHub's installation tokens expire after an hour. Each push gets a new token with Contents write, and Workflows write only when the task's change touches a file under `.github/workflows/`; the clone and the API calls share one, which CAVU keeps until five minutes before it expires. With Checks read an App also reads the check runs, which a fine-grained personal access token cannot (below). Between them, CAVU's tokens ask for every permission in the App's column, and GitHub's documentation says an installation token cannot be granted a permission the App was not granted, nor access to a repository its installation was not. Install the App on the repository, then set `forge_app_id` to the App's id and store its private key, in PEM, as `forge_app_key`:

```
cavu settings set forge_app_id 123456
cavu secret set forge_app_key < app-key.pem
```

With `forge_app_id` empty, CAVU uses the personal access token `forge_token`, as it is, for every clone, push and call. A fine-grained personal access token limited to the repository needs only the permissions in its column: GitHub's documentation names each of them for one of CAVU's calls, and offers Workflows as write, and write always includes read. A classic personal access token's `repo` scope is what GitHub's documentation describes as full access to public and private repositories, so it reaches every repository the account can, not this one alone. Then store the token:

```
cavu secret set forge_token < token.txt
```

A project can name another credential, for example one per organisation. Store it under a name from standard input, a personal access token or a GitHub App's private key in PEM, then name it on the project; `--forge-credential ''` returns the project to the default. The table applies to each credential.

```
cavu credential set acme-org < token.txt
cavu credential set work-app --app-id 482913 < app-key.pem
cavu project set web --forge-credential acme-org
```

`cavu credential list` lists the credentials with their kind and an App's id, never a secret, and `cavu credential remove NAME` removes one that no project names.

GitHub's documentation lists calling the Checks API among what fine-grained tokens do not support, while a classic token with `repo` reads the check runs. When GitHub refuses a personal access token the Checks API, CAVU reads the jobs of the head commit's GitHub Actions workflow runs and the commit statuses instead, and a check reported only as a check run, by another app or by a workflow step, is not seen: CAVU neither waits for it nor acts on its result. A pull request whose only checks are such check runs reads as having none, so CAVU marks it ready after 3 minutes and stops it as `no_checks` 3 minutes later, or with `--no-ci on` marks it ready at once. For the GraphQL calls with which CAVU reads the reviews and marks the pull request ready for review, GitHub's documentation of the permissions fine-grained tokens need names none, since it covers the REST routes only; its guide to GraphQL calls says that the data a call asks for decides the permissions it needs, and that the answer to a call without them names those the token needs.

GitHub's documentation names the Workflows permission, or the `workflow` scope, for editing workflow files. Without it, when GitHub refuses the push, the task stops as `push_rejected` and the explanation quotes the reason; when GitHub answers the request for an App's push token with status 422, the task stops the same way and the explanation names the permissions the App must be granted.
