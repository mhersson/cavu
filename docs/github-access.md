# GitHub access

CAVU acts on GitHub with a GitHub App or a personal access token, fine-grained or classic.

## Choosing a method

A GitHub App is recommended: CAVU asks the App's installation on the repository for tokens for that repository alone, each with only what its use needs, and GitHub's installation tokens expire after an hour. Each push gets a new token with Contents write, and Workflows write only when the task's change touches a file under `.github/workflows/`; the clone and the API calls share one, which CAVU keeps until five minutes before it expires. With Checks read an App also reads the check runs, which a fine-grained personal access token cannot ([below](#checks-and-a-fine-grained-token)). CAVU signs every request for a token with the App's private key, which GitHub's documentation says does not expire.

CAVU uses a personal access token as it is, for every clone, push and call, and does not renew it. GitHub's documentation says such a token is meant to act on behalf of the person who made it, and recommends a GitHub App for an organisation's resources and for long-lived integrations. It also says a fine-grained token can be limited to the repositories CAVU works on, and that a classic one can access every repository its owner can ([A classic token](#a-classic-token)).

## Permissions

The App or token you make needs this access to the repository CAVU works on:

| Permission | GitHub App | Fine-grained token | Classic token (scope) | What CAVU does with it |
| --- | --- | --- | --- | --- |
| Contents | Read and write | Read and write | `repo` | Clones the repository and pushes the task's branch |
| Pull requests | Read and write | Read and write | `repo` | Opens the pull request as a draft, reads it with its reviews, marks it ready for review and comments on it |
| Checks | Read-only | Not available ([below](#checks-and-a-fine-grained-token)) | `repo` | Reads the check runs of the pull request's newest commit |
| Commit statuses | Read-only | Read-only | `repo` | Reads the commit statuses of the pull request's newest commit |
| Actions | Read-only | Read-only | `repo` | Reads the end of a failed GitHub Actions job's log for a fix run, and, once GitHub has refused the Checks API, the jobs of the newest commit's workflow runs in place of its check runs |
| Metadata | Read-only | Read-only | `repo` | Reads the repository, and a reviewer's permission on it, which decides [whose requests count](security.md) |
| Workflows | Read and write | Read and write | `workflow` | Only for a change to a file under `.github/workflows/`: pushes it |

CAVU clones the repository with the credential, which a private repository needs, and does everything else in the last column only with a project's switch `publish` on, the default, on a public repository too ([The pull request](tasks.md#the-pull-request)). The credential you set up below is the default credential, which every project uses unless it names another ([A credential per project](#a-credential-per-project)).

## A GitHub App

### Create the App

1. On GitHub, open the settings of the account or organisation that owns the repositories: **Settings** for your own account, or **Your organizations** and then **Settings** beside the organisation. Open **Developer settings**, then **GitHub Apps**, and press **New GitHub App**.
2. Give it a **GitHub App name**, one not taken on GitHub, such as `cavu-acme-org`. GitHub's documentation says GitHub shows the name when the App acts, and CAVU acts on GitHub as the App's bot ([Tasks](tasks.md)).
3. Give a **Homepage URL**: GitHub's documentation says the address of the account that owns the App will do.
4. Under **Webhook**, clear **Active**: CAVU takes no webhook, since `cavu serve` looks at each pull request itself ([The pull request](tasks.md#the-pull-request)).
5. Under **Permissions**, give each repository permission its value in the App's column of the table, and leave every organisation and account permission at **No access**. Between them, CAVU's tokens ask for every permission in the App's column, and GitHub's documentation says an installation token cannot be granted a permission the App was not granted, nor access to a repository its installation was not.
6. Under **Where can this GitHub App be installed?**, choose **Only on this account**: GitHub's documentation says such an App installs only on the account that made it, which is enough when that account owns the repositories.
7. Leave the other fields as they are, since CAVU uses only the App's installation tokens, and press **Create GitHub App**.

[GitHub's documentation on registering a GitHub App](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/registering-a-github-app) describes every field. The App's settings page is under **Developer settings**, **GitHub Apps**, and **Edit** beside the App.

### Generate the private key

On the App's settings page, open **Credentials**, then **Key pairs**, and press **New key**. GitHub's documentation says the browser then downloads the private key in PEM, and that GitHub keeps only its public half, so keep the file for CAVU. [GitHub's documentation on private keys](https://docs.github.com/en/apps/creating-github-apps/authenticating-with-a-github-app/managing-private-keys-for-github-apps) has the steps.

### Install the App

1. On the App's settings page, press **Install App**, then **Install** beside the account or organisation that owns the repositories.
2. Choose **Only select repositories**, pick the repositories CAVU works on, and press **Install**.

[GitHub's documentation on installing your own GitHub App](https://docs.github.com/en/apps/using-github-apps/installing-your-own-github-app) has the steps. To add a repository later, press **Configure** beside the App where GitHub lists the installed GitHub Apps (for an organisation, its **Settings**, then **GitHub Apps** under **Third-party Access**; for your own account, **Settings**, **Applications**, then **Installed GitHub Apps**), pick it under **Repository access** and press **Save**, as [GitHub's documentation on installed GitHub Apps](https://docs.github.com/en/apps/using-github-apps/reviewing-and-modifying-installed-github-apps) says.

### Give it to CAVU

Set `forge_app_id` to the App's id, the App ID its settings page shows, which GitHub's documentation says is not its client ID, and store its private key, in PEM, as `forge_app_key`. Do it in onboarding's GitHub step ([Getting started](../README.md#getting-started), step 7); on the **Settings** page, under **GitHub**, with **Set** or **Replace** on the **default** row and **A GitHub App**; or with these commands:

```
cavu settings set forge_app_id 123456
cavu secret set forge_app_key < app-key.pem
```

CAVU finds the App's installation from the repository, so it needs no installation id.

### When the App's permissions change

To add a permission later, such as Workflows, change it under **Permissions & events** on the App's settings page and press **Save changes**. GitHub's documentation says GitHub then emails the owner of each account the App is installed on, that the new permission takes effect on an installation only once that account approves it, and that until then the installation keeps its old permissions. Approve it on every installation ([GitHub's documentation on approving updated permissions](https://docs.github.com/en/apps/using-github-apps/approving-updated-permissions-for-a-github-app)).

## A personal access token

[GitHub's documentation on managing personal access tokens](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/managing-your-personal-access-tokens) has the steps for both kinds.

### A fine-grained token

1. On GitHub, open **Settings**, then **Developer settings**, then **Fine-grained tokens** under **Personal access tokens**, and press **Generate new token**.
2. Give it a **Token name** and an **Expiration**. Before it expires, make a new one and store it as below. GitHub's documentation says a token may have no expiry, unless an organisation or enterprise sets a maximum lifetime, which for an organisation is 366 days by default.
3. As **Resource owner**, choose the account or organisation that owns the repositories: GitHub's documentation says the token reaches only what that owner owns. It also says that in an organisation that requires approval of fine-grained tokens, the default, a token is `pending` and reads only public resources until an owner of the organisation approves it, and that an owner's own token is approved at once.
4. Under **Repository access**, choose **Only select repositories** and the repositories CAVU works on.
5. Under **Permissions**, select the permissions of the table, since a fine-grained personal access token limited to the repository needs only the permissions in its column: GitHub's documentation names each of them for one of CAVU's calls, and offers Workflows as write, and write always includes read.
6. Press **Generate token** and copy the token.

Then store the token, which CAVU uses while `forge_app_id` is empty: in onboarding's GitHub step ([Getting started](../README.md#getting-started), step 7); on the **Settings** page, under **GitHub**, with **Set** or **Replace** on the **default** row and **A token**; or with this command:

```
cavu secret set forge_token < token.txt
```

For the GraphQL calls with which CAVU reads the reviews and marks the pull request ready for review, GitHub's documentation of the permissions fine-grained tokens need names none, since it covers the REST routes only; its guide to GraphQL calls says that the data a call asks for decides the permissions it needs, and that the answer to a call without them names those the token needs.

### A classic token

On GitHub, open **Settings**, then **Developer settings**, then **Tokens (classic)** under **Personal access tokens**, and choose **Generate new token**, then **Generate new token (classic)**. Give it a **Note** and an **Expiration**, select the scope `repo`, and `workflow` for a change to a workflow, press **Generate token**, and store the token as a fine-grained one is stored. A classic personal access token's `repo` scope is what GitHub's documentation describes as full access to public and private repositories, so it reaches every repository the account can, not this one alone. GitHub's documentation says some access needs a classic token, such as an outside collaborator's to an organisation's repositories, and that a token for an organisation that uses SAML single sign-on must be authorized for it.

## A credential per project

A project can name another credential, for example one per organisation. Store it under a name from standard input, a personal access token or a GitHub App's private key in PEM, then name it on the project; `--forge-credential ''` returns the project to the default. The table applies to each credential.

```
cavu credential set acme-org < token.txt
cavu credential set work-app --app-id 482913 < app-key.pem
cavu project set web --forge-credential acme-org
```

`cavu credential list` lists the credentials with their kind and an App's id, never a secret, and `cavu credential remove NAME` removes one that no project names.

In the web app, **Add a credential** on the **Settings** page under **GitHub** stores one with its **Name** and **Kind**. A new project names its credential as its **GitHub credential**, which the form shows once a named credential exists, and a project's page under **GitHub** changes it as its **Credential**, with **Edit** and **Save**.

## Checks and a fine-grained token

GitHub's documentation lists calling the Checks API among what fine-grained tokens do not support, while a classic token with `repo` reads the check runs. When GitHub refuses a personal access token the Checks API, CAVU reads the jobs of the head commit's GitHub Actions workflow runs and the commit statuses instead, and a check reported only as a check run, by another app or by a workflow step, is not seen: CAVU neither waits for it nor acts on its result. A pull request whose only checks are such check runs reads as having none, so CAVU marks it ready after 3 minutes and stops it as `no_checks` 3 minutes later, or with `--no-ci on` marks it ready at once.

## GitHub Enterprise

A project's repository can be on github.com or on GitHub Enterprise. CAVU finds GitHub's API from the host of the repository's address, with no setting: `https://api.github.com` for `github.com`, `https://api.HOST` for a host under `.ghe.com`, and `https://HOST/api/v3` for any other host. Its calls to the API trust the CA bundle that `ca_bundle_path` names too, beside the system's certificates.

## The Workflows permission

GitHub's documentation names the Workflows permission, or the `workflow` scope, for editing workflow files. Without it, when GitHub refuses the push, the task stops as `push_rejected` and the explanation quotes the reason; when GitHub answers the request for an App's push token with status 422, the task stops the same way and the explanation names the permissions the App must be granted. Grant it, and for an App approve it on each installation ([When the App's permissions change](#when-the-apps-permissions-change)), then retry the task ([The pull request](tasks.md#the-pull-request)).

## Checking it

Run `cavu doctor` once a project exists, while `cavu serve` is stopped. Its `forge` line for each project reads `ok`, `skipped` when the project has no credential, or `FAIL` with the error and, under it, a `fix:` line that says what to check. [The doctor](configuration.md#the-doctor) says what the forge check does.

The environment check ([Getting started](../README.md#getting-started), step 9) clones the repository with the project's credential, as each run does, so a failed `clone` step there can be the credential's. `cavu project check web` runs it from the command line.

## Common mistakes

- **The App is not installed on the repository**, or the repository is not among its installation's repositories. The doctor's forge line and the environment check's `clone` step fail with an error that names CAVU's request for a token, to `/repos/OWNER/REPO/installation` or `/app/installations/ID/access_tokens` under GitHub's API, and quotes GitHub's answer. Install the App on the repository ([Install the App](#install-the-app)).
- **A permission added to the App and not yet approved on an installation.** The installation keeps its old permissions ([above](#when-the-apps-permissions-change)). When GitHub refuses CAVU a token for that reason, the doctor's forge line and the `clone` step fail as above, or, for a push's token refused with status 422, the task stops as `push_rejected` ([The Workflows permission](#the-workflows-permission)). Approve the change on the installation.
- **`forge_app_id` set with no private key, or the wrong file as the key.** Without the key, the doctor's forge line fails with `secret forge_app_key is not set`, the **default** row on the **Settings** page reads "A GitHub App, id 123456, its key is not set", and a task stops as `sandbox` with `The run cannot be configured: secret forge_app_key is not set`. A file that is not a private key in PEM fails with an error that begins `forge: the GitHub App's private key:`. Store the file the App's settings page gave ([Generate the private key](#generate-the-private-key)) as `forge_app_key`.
- **The client ID in place of the App ID** ([Give it to CAVU](#give-it-to-cavu)). `forge_app_id` is a whole number, and CAVU refuses the client ID: `cavu settings set` says `setting forge_app_id: "VALUE" is not a whole number of 1 or more`, and onboarding and the **Settings** page refuse it too.
- **A fine-grained token that does not reach the repository**: the repository is not under its **Repository access**, its **Resource owner** is not the repository's owner, or it waits for the organisation's approval ([A fine-grained token](#a-fine-grained-token)). For a private repository, the doctor's forge line and the environment check's `clone` step fail. GitHub's documentation says every fine-grained token can read a public repository, so there the clone passes, and a push the token may not make stops the task as `push_rejected`, the explanation quoting the reason.
- **A fine-grained token without Actions or Commit statuses read.** Once GitHub has refused the token the Checks API, CAVU reads Actions jobs and commit statuses ([Checks and a fine-grained token](#checks-and-a-fine-grained-token)). When GitHub refuses those too, the doctor's forge line fails and its `fix:` line says to grant read access to Actions and Commit statuses, and the page of a task whose pull request CAVU watches says that the last look at the pull request failed, with the error and the time of the next look. Grant both.
- **A task that changes a workflow, with no Workflows permission**: see [The Workflows permission](#the-workflows-permission).
- **An expired token.** GitHub's documentation says an expired token can no longer be used for Git and API requests. For a private repository, the doctor's forge line fails and a task's run stops as `environment` at the step `clone`; for a public one the clone passes, and the push stops the task as `push_rejected`. The looks at the pull requests CAVU watches fail. Make a new token and store it as the first ([A personal access token](#a-personal-access-token)).
- **A token stored while `forge_app_id` is set.** With `forge_app_id` set, CAVU uses the App and not `forge_token`, and the **default** row on the **Settings** page names the App. `cavu secret set forge_token` alone does not switch to the token: unset the App too, with `cavu settings set forge_app_id ''`. Onboarding's GitHub step and the **Settings** page do this themselves: storing a token there unsets `forge_app_id` and removes `forge_app_key`, and storing an App's key removes `forge_token`.
