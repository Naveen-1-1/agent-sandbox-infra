# The AWS environment (aws branch): EKS, S3 and ECR from infra/terraform,
# the platform on top, the benchmark on it, and a checked teardown. Nothing
# is created or destroyed without a saved plan and APPROVED=yes; see
# hack/aws.sh. It uses your AWS CLI credentials and bin/kubeconfig-aws.

##@ AWS

AWS_ENV = RUN=$(RUN) AWS_REGION=$(AWS_REGION) AWS_NODES=$(AWS_NODES) AWS_CAPACITY=$(AWS_CAPACITY) \
  AWS_INSTANCE_TYPE=$(AWS_INSTANCE_TYPE) APPROVED=$(APPROVED)
# The AWS CLI profile's region (accounts are often limited to one), else us-east-1.
AWS_REGION   ?= $(or $(shell aws configure get region 2>/dev/null),us-east-1)
AWS_NODES    ?= 2
AWS_CAPACITY ?= ON_DEMAND
# Accounts on the AWS Free plan only run Free Tier types: use m7i-flex.large.
AWS_INSTANCE_TYPE ?= m7g.large

.PHONY: aws-plan
aws-plan: ## Plan the AWS environment and estimate its hourly cost; changes nothing (RUN, AWS_NODES, AWS_CAPACITY=SPOT).
	@$(AWS_ENV) hack/aws.sh plan

.PHONY: aws-up
aws-up: ## Create the planned environment and install the platform on it (APPROVED=yes).
	@$(AWS_ENV) hack/aws.sh apply
	@$(AWS_ENV) hack/aws.sh deploy

.PHONY: aws-deploy
aws-deploy: ## Push the images and install the platform on the existing environment again.
	@$(AWS_ENV) hack/aws.sh deploy

.PHONY: aws-bench
aws-bench: ## Run the benchmark on EKS; writes notes/bench-aws-<date>.json and notes/aws-modeled-<run>.json.
	@$(AWS_ENV) hack/aws.sh bench $(BENCH_ARGS)

.PHONY: aws-down-plan
aws-down-plan: ## Plan the teardown; changes nothing.
	@$(AWS_ENV) hack/aws.sh down-plan

.PHONY: aws-down
aws-down: ## Delete the workspaces, uninstall, destroy the planned teardown and check nothing is left (APPROVED=yes).
	@$(AWS_ENV) hack/aws.sh down

.PHONY: aws-verify
aws-verify: ## Check that nothing of a run is left in AWS (RUN=<run>).
	@$(AWS_ENV) hack/aws.sh verify

.PHONY: aws-cost-actual
aws-cost-actual: ## What Cost Explorer billed the run, a day later (FROM=YYYY-MM-DD); writes notes/aws-cost-<run>.json.
	@$(AWS_ENV) FROM=$(FROM) TO=$(TO) hack/aws.sh cost-actual
