CREATE TABLE users (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email      text NOT NULL UNIQUE,
    role       text NOT NULL DEFAULT 'user' CHECK (role IN ('user', 'admin')),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Which roles may deploy which template (templates themselves live as JSON in Git).
CREATE TABLE template_assignments (
    template_id text NOT NULL,
    role        text NOT NULL,
    PRIMARY KEY (template_id, role)
);

CREATE TABLE deployments (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id     text NOT NULL,
    module_ref      text NOT NULL,          -- approved module revision used for this deployment
    inputs          jsonb NOT NULL,
    status          text NOT NULL DEFAULT 'pending',
    pipeline_run_id bigint,
    state_key       text NOT NULL UNIQUE,   -- reference to remote Terraform state, never the state itself
    outputs         jsonb,
    created_by      uuid REFERENCES users(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE deployment_status_history (
    id            bigserial PRIMARY KEY,
    deployment_id uuid NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    status        text NOT NULL,
    message       text,
    created_at    timestamptz NOT NULL DEFAULT now()
);
