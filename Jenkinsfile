pipeline {
    agent { 
        node {label 'bare-metal' }
    } 
    stages {
        stage('pre-cleanup') {
            steps {
                sh '''#!/bin/bash
                base_dir=`pwd`

                cd $base_dir/Skyport2
                rm -f docker-compose.yaml
                ln -s Docker/Compose/skyport-awe-testing.yaml docker-compose.yaml

                # Clean up
                if [ -e ./skyport2.env ] ; then
                  . ./skyport2.env
                  docker-compose down
                fi
                docker rm -f $(docker ps -a -f name=skyport2_ -q)
                docker rm -f $(docker ps -a -f name=compose_ -q)
                docker rm -f mgrast_cwl_submitter


                echo "Deleting live-data"

                docker run --rm --volume `pwd`:/tmp/workspace bash rm -rf /tmp/workspace/live-data
                # ls -l live-data
                docker volume prune -f
                '''
            }
        }
        stage('Build') { 
            steps {
                sh '''#!/bin/bash
                
                echo "SHELL=$SHELL"
                echo "HOSTNAME=$HOSTNAME"


                export PATH="/usr/local/bin/:$PATH"
                echo "PATH=$PATH"
                DOCKER_PATH=$(which docker)
                echo "DOCKER_PATH=${DOCKER_PATH}"



                # Debugging
                pwd
                ls -l
                # docker images
                docker ps

                base_dir=`pwd`

               
                docker ps
                set -x
                sudo ./scripts/add_etc_hosts_entry.sh

                source ./init.sh
                #. ./skyport2.env

                if [ ${SKYPORT_DOCKER_GATEWAY}x == x ] ; then
                  set +e
                  exit 1
                fi

                # Build container
                cd $base_dir/AWE
                set +x
                echo Building AWE Container
                set -x

                #git branch -v
                #git remote -v
                #git describe

                USE_CACHE="--no-cache"
                USE_CACHE="" #speed-up for debugging purposes 

                docker build ${USE_CACHE} -t mgrast/awe:test -f Dockerfile .
                docker build ${USE_CACHE} -t mgrast/awe-worker:test -f Dockerfile_worker .
                docker build ${USE_CACHE} -t mgrast/awe-submitter:test -f Dockerfile_submitter .
                cd $base_dir/Skyport2
                docker run --rm --volume `pwd`:/Skyport2 bash rm -rf /Skyport2/tmp
                docker build ${USE_CACHE} -t mgrast/cwl-submitter:test -f Docker/Dockerfiles/cwl-test-submitter.dockerfile .

                echo "docker builds complete"
                sleep 5

                docker ps

                sleep 1

                if [ ${SKYPORT_DOCKER_GATEWAY}x == x ] ; then
                  set +e
                  exit 1
                fi

                echo "SKYPORT_DOCKER_GATEWAY: ${SKYPORT_DOCKER_GATEWAY}"
                export SKYPORT_DOCKER_GATEWAY=${SKYPORT_DOCKER_GATEWAY}
                docker-compose up -d
                '''
            }
        }
        stage('Test') { 
            steps {
                sh '''#!/bin/bash
                set -x
            
                base_dir=`pwd`
                cd $base_dir
                touch result.xml
                docker run \
                    --rm \
                    --network skyport2_default \
                    --name mgrast_cwl_submitter \
                    --volume `pwd`/result.xml:/output/result.xml \
                    mgrast/cwl-submitter:test \
                    --junit-xml=/output/result.xml \
                    --timeout=120
                '''
            }
        }
    }
    post {
        always {
            sh '''#!/bin/bash
            set -x
            # Clean up
            base_dir=`pwd`
            cd $base_dir/Skyport2
            docker-compose down
            docker run --rm --volume `pwd`/live-data:/live-data bash rm -rf /live-data/*
            docker volume prune -f
            '''
        }        
    }
}